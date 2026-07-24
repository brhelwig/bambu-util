package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/brhelwig/bambu-util/internal/history"
	"github.com/brhelwig/bambu-util/internal/p1s"
)

//go:embed static
var staticFS embed.FS

// Commander is what the HTTP layer needs from the printer link.
type Commander interface {
	LowerBed()
	Home()
	SetBedTemp(int)
	SetNozzleTemp(int)
	Extrude()
	UnloadFilament()
	LoadFilament(slot, currTemp, tarTemp int)
	SetAmsFilament(amsID, trayID int, trayInfoIdx, color, trayType string, tempMin, tempMax int)
	SetChamberLight(bool)
	PausePrint()
	ResumePrint()
	StopPrint()
}

type Server struct {
	cache   *p1s.StateCache
	cmd     Commander
	store   *history.Store
	autoOff *autoOff
	lamp    *lampAuto
}

func NewServer(cache *p1s.StateCache, cmd Commander, store *history.Store) *Server {
	return &Server{cache: cache, cmd: cmd, store: store, autoOff: newAutoOff(), lamp: newLampAuto()}
}

// EnforceAutoOff runs the heater safety shut-off loop until ctx is cancelled.
// Call once (from main); the HTTP handlers do not need it to serve countdowns.
func (s *Server) EnforceAutoOff(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if bed, nozzle := s.autoOff.due(); bed || nozzle {
				if bed {
					s.cmd.SetBedTemp(0)
				}
				if nozzle {
					s.cmd.SetNozzleTemp(0)
				}
			}
		}
	}
}

// EnforceLampAutomation runs the chamber-lamp automation loop until ctx is
// cancelled. Call once (from main).
func (s *Server) EnforceLampAutomation(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.pollLamp()
		}
	}
}

func (s *Server) pollLamp() {
	fields, connected := s.cache.Snapshot()
	if !connected {
		return
	}
	gs := p1s.GcodeState(fields)
	jobActive := gs == "RUNNING" || gs == "PAUSE"
	bedTarget, _ := fields["bed_target_temper"].(float64)
	nozzleTarget, _ := fields["nozzle_target_temper"].(float64)
	active := jobActive || bedTarget > 0 || nozzleTarget > 0

	// forceOn/forceOff each fire exactly once, on the relevant transition
	// (see lampAuto), so there's no need to read the printer's reported
	// lamp state first to dedup — nothing here polls or spams a command
	// every tick, only on an actual transition.
	forceOn, forceOff := s.lamp.poll(active)
	if forceOn {
		s.cmd.SetChamberLight(true)
	} else if forceOff {
		s.cmd.SetChamberLight(false)
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	files := http.FileServerFS(static)
	// Embedded files carry no modtime, so serve them no-cache: revalidation
	// is cheap and stale pages on phones are worse.
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	}))
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("POST /api/actions/{name}", s.action)
	mux.HandleFunc("GET /camera/history/range", s.historyRange)
	mux.HandleFunc("GET /camera/history/frame", s.historyFrame)
	mux.HandleFunc("GET /camera/history/jobs", s.historyJobs)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	fields, connected := s.cache.Snapshot()
	gs := p1s.GcodeState(fields)
	bedOff, nozzleOff := s.autoOff.remaining()
	resp := map[string]any{
		"connected":        connected,
		"gcodeState":       gs,
		"actionsAllowed":   p1s.ActionAllowed(connected, gs) == nil,
		"bedTemp":          fields["bed_temper"],
		"bedTarget":        fields["bed_target_temper"],
		"nozzleTemp":       fields["nozzle_temper"],
		"nozzleTarget":     fields["nozzle_target_temper"],
		"progress":         fields["mc_percent"],
		"jobName":          p1s.JobName(fields),
		"layerNum":         fields["layer_num"],
		"totalLayerNum":    fields["total_layer_num"],
		"remainingMinutes": fields["mc_remaining_time"],
		"chamberTemp":      fields["chamber_temper"],
		"chamberLight":     p1s.ChamberLight(fields),
		"wifiSignal":       fields["wifi_signal"],
		"fans": map[string]any{
			"cooling": fields["cooling_fan_speed"],
			"aux":     fields["big_fan1_speed"],
			"chamber": fields["big_fan2_speed"],
		},
		"ams":         fields["ams"],
		"hms":         p1s.HMSErrors(fields),
		"bedOffIn":    nilIfNeg(bedOff),
		"nozzleOffIn": nilIfNeg(nozzleOff),
		"lampOffIn":   nilIfNeg(s.lamp.remaining()),
		"printActions": map[string]bool{
			"pause":  p1s.PrintActionAllowed(connected, gs, "pause") == nil,
			"resume": p1s.PrintActionAllowed(connected, gs, "resume") == nil,
			"stop":   p1s.PrintActionAllowed(connected, gs, "stop") == nil,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

var actions = map[string]func(Commander){
	"lower-bed": func(c Commander) { c.LowerBed() },
	"home":      func(c Commander) { c.Home() },
	"unload":    func(c Commander) { c.UnloadFilament() },
}

// Bounds for the parameterized temperature sliders. The P1S bed tops out
// near 100°C and the nozzle near 300°C; the small headroom just guards the
// top preset against rounding.
const (
	BedMaxTemp    = 110
	NozzleMaxTemp = 300
)

func parseTemp(raw string, max int) (int, error) {
	t, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid temp %q", raw)
	}
	if t < 0 || t > max {
		return 0, fmt.Errorf("temp %d out of range 0-%d", t, max)
	}
	return t, nil
}

var printActions = map[string]func(Commander){
	"pause":  func(c Commander) { c.PausePrint() },
	"resume": func(c Commander) { c.ResumePrint() },
	"stop":   func(c Commander) { c.StopPrint() },
}

func (s *Server) action(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	fields, connected := s.cache.Snapshot()
	gs := p1s.GcodeState(fields)

	// The drying and nozzle sliders post an arbitrary target, handled
	// separately from the fixed actions map.
	switch name {
	case "set-bed-temp":
		s.setTemp(w, r, connected, gs, name, BedMaxTemp, s.cmd.SetBedTemp, s.autoOff.setBed)
		return
	case "set-nozzle-temp":
		s.setTemp(w, r, connected, gs, name, NozzleMaxTemp, s.cmd.SetNozzleTemp, s.autoOff.setNozzle)
		return
	case "extrude":
		s.extrude(w, connected, gs, fields)
		return
	case "load":
		s.load(w, r, connected, gs, fields)
		return
	case "set-filament":
		s.setFilament(w, r, connected, gs)
		return
	case "lamp-on", "lamp-off":
		// The chamber light is safe to toggle in any state, so it skips the
		// idle guard; it only needs the printer reachable.
		if !connected {
			http.Error(w, "blocked: not connected to printer", http.StatusConflict)
			return
		}
		s.cmd.SetChamberLight(name == "lamp-on")
		fmt.Fprintf(w, "sent: %s", name)
		return
	}

	act, ok := actions[name]
	if ok {
		if !guardIdle(w, connected, gs) {
			return
		}
	} else if act, ok = printActions[name]; ok {
		if err := p1s.PrintActionAllowed(connected, gs, name); err != nil {
			http.Error(w, "blocked: "+err.Error(), http.StatusConflict)
			return
		}
	} else {
		http.Error(w, "unknown action", http.StatusNotFound)
		return
	}
	act(s.cmd)
	fmt.Fprintf(w, "sent: %s", name)
}

// guardIdle writes a 409 and returns false when a bed/nozzle/AMS action isn't
// currently allowed (disconnected or mid-print). Shared by every idle-only
// command so the guard is wired in exactly one place.
func guardIdle(w http.ResponseWriter, connected bool, gs string) bool {
	if err := p1s.ActionAllowed(connected, gs); err != nil {
		http.Error(w, "blocked: "+err.Error(), http.StatusConflict)
		return false
	}
	return true
}

// setTemp handles the parameterized set-bed-temp / set-nozzle-temp endpoints:
// parse and range-check the temp, apply the idle guard, then send it.
func (s *Server) setTemp(w http.ResponseWriter, r *http.Request, connected bool, gs, name string, max int, set, record func(int)) {
	temp, err := parseTemp(r.URL.Query().Get("temp"), max)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !guardIdle(w, connected, gs) {
		return
	}
	set(temp)
	record(temp) // arm/reset/cancel the safety auto-off
	fmt.Fprintf(w, "sent: %s %d", name, temp)
}

// nilIfNeg maps the auto-off "inactive" sentinel (-1) to JSON null and passes
// real second counts through unchanged.
func nilIfNeg(secs int) any {
	if secs < 0 {
		return nil
	}
	return secs
}

// ExtrudeMinTemp guards manual extrusion: pushing filament through a cold
// nozzle strips the filament and can jam the extruder. Bambu's firmware blocks
// cold extrusion too; this matches it defensively.
const ExtrudeMinTemp = 170

func (s *Server) extrude(w http.ResponseWriter, connected bool, gs string, fields map[string]any) {
	if !guardIdle(w, connected, gs) {
		return
	}
	nt, ok := fields["nozzle_temper"].(float64)
	if !ok || nt < ExtrudeMinTemp {
		http.Error(w, fmt.Sprintf("blocked: nozzle below %d°C", ExtrudeMinTemp), http.StatusConflict)
		return
	}
	s.cmd.Extrude()
	fmt.Fprint(w, "sent: extrude")
}

// MaxAMSSlot is the highest global tray index Load accepts. Bambu supports up
// to 4 AMS units of 4 trays, addressed as ams_id*4 + tray_id (0-15).
const MaxAMSSlot = 15

// load feeds an AMS tray (?slot=0-15, the global ams_id*4+tray_id index) into
// the hotend. Per the chosen design it heats to whatever nozzle target the user
// set via the slider, so it refuses when no nozzle target is set.
func (s *Server) load(w http.ResponseWriter, r *http.Request, connected bool, gs string, fields map[string]any) {
	slot, err := strconv.Atoi(r.URL.Query().Get("slot"))
	if err != nil || slot < 0 || slot > MaxAMSSlot {
		http.Error(w, "invalid slot", http.StatusBadRequest)
		return
	}
	if !guardIdle(w, connected, gs) {
		return
	}
	tar, _ := fields["nozzle_target_temper"].(float64)
	if tar <= 0 {
		http.Error(w, "blocked: set a nozzle temperature first", http.StatusConflict)
		return
	}
	cur, _ := fields["nozzle_temper"].(float64)
	s.cmd.LoadFilament(slot, int(cur), int(tar))
	fmt.Fprintf(w, "sent: load slot %d", slot)
}

// MaxAMSUnit is the highest AMS unit index accepted (Bambu supports up to 4
// units, addressed 0-3).
const MaxAMSUnit = 3

// setFilament writes a tray's filament profile (ams_filament_setting). It's a
// full-tray write, so the client resends the tray's existing type/temps/idx
// alongside the field it changed — otherwise the printer would blank them.
func (s *Server) setFilament(w http.ResponseWriter, r *http.Request, connected bool, gs string) {
	q := r.URL.Query()
	amsID, err := parseIndex(q.Get("ams_id"), MaxAMSUnit)
	if err != nil {
		http.Error(w, "invalid ams_id", http.StatusBadRequest)
		return
	}
	trayID, err := parseIndex(q.Get("tray_id"), 3)
	if err != nil {
		http.Error(w, "invalid tray_id", http.StatusBadRequest)
		return
	}
	color, err := normalizeColor(q.Get("tray_color"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	typ := q.Get("tray_type")
	if typ == "" || len(typ) > 32 {
		http.Error(w, "invalid tray_type", http.StatusBadRequest)
		return
	}
	tmin, err := parseTemp(q.Get("nozzle_temp_min"), NozzleMaxTemp)
	if err != nil {
		http.Error(w, "invalid nozzle_temp_min", http.StatusBadRequest)
		return
	}
	tmax, err := parseTemp(q.Get("nozzle_temp_max"), NozzleMaxTemp)
	if err != nil {
		http.Error(w, "invalid nozzle_temp_max", http.StatusBadRequest)
		return
	}
	if tmin > tmax {
		http.Error(w, "nozzle_temp_min above nozzle_temp_max", http.StatusBadRequest)
		return
	}
	idx := q.Get("tray_info_idx")
	if len(idx) > 32 {
		http.Error(w, "invalid tray_info_idx", http.StatusBadRequest)
		return
	}
	if !guardIdle(w, connected, gs) {
		return
	}
	s.cmd.SetAmsFilament(amsID, trayID, idx, color, typ, tmin, tmax)
	fmt.Fprintf(w, "sent: set-filament ams %d tray %d", amsID, trayID)
}

// parseIndex parses a small non-negative index bounded by max (inclusive).
func parseIndex(raw string, max int) (int, error) {
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 || n > max {
		return 0, fmt.Errorf("index out of range")
	}
	return n, nil
}

// normalizeColor accepts a 6- or 8-hex-digit colour and returns it as uppercase
// RRGGBBAA (alpha forced to FF when only RRGGBB is given), the form the AMS
// reports and expects.
func normalizeColor(raw string) (string, error) {
	up := strings.ToUpper(raw)
	if len(up) == 6 {
		up += "FF"
	}
	if len(up) != 8 {
		return "", fmt.Errorf("tray_color must be RRGGBB or RRGGBBAA hex")
	}
	for _, c := range up {
		if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'F') {
			return "", fmt.Errorf("tray_color must be hex")
		}
	}
	return up, nil
}

func (s *Server) historyRange(w http.ResponseWriter, _ *http.Request) {
	oldest, newest, err := s.store.Range()
	if err != nil {
		http.Error(w, "range query failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"oldest": oldest, "newest": newest})
}

func (s *Server) historyFrame(w http.ResponseWriter, r *http.Request) {
	ts, err := strconv.ParseInt(r.URL.Query().Get("ts"), 10, 64)
	if err != nil {
		http.Error(w, "invalid ts", http.StatusBadRequest)
		return
	}
	jpeg, _, err := s.store.FrameAtOrAfter(ts)
	if errors.Is(err, history.ErrNoFrame) {
		http.Error(w, "no frame at or after ts", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "frame query failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Write(jpeg)
}

func (s *Server) historyJobs(w http.ResponseWriter, _ *http.Request) {
	jobs, err := s.store.RecentJobs()
	if err != nil {
		http.Error(w, "jobs query failed", http.StatusInternalServerError)
		return
	}
	out := make([]map[string]any, len(jobs))
	for i, j := range jobs {
		out[i] = map[string]any{"id": j.ID, "name": j.Name, "start": j.Start, "end": j.End}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}
