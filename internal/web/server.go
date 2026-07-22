package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"time"

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
	PausePrint()
	ResumePrint()
	StopPrint()
}

type Server struct {
	cache   *p1s.StateCache
	cmd     Commander
	hub     *Hub
	autoOff *autoOff
}

func NewServer(cache *p1s.StateCache, cmd Commander, hub *Hub) *Server {
	return &Server{cache: cache, cmd: cmd, hub: hub, autoOff: newAutoOff()}
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
	mux.HandleFunc("GET /camera/stream", s.camera)
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
	}

	var guardErr error
	act, ok := actions[name]
	if ok {
		guardErr = p1s.ActionAllowed(connected, gs)
	} else if act, ok = printActions[name]; ok {
		guardErr = p1s.PrintActionAllowed(connected, gs, name)
	} else {
		http.Error(w, "unknown action", http.StatusNotFound)
		return
	}
	if guardErr != nil {
		http.Error(w, "blocked: "+guardErr.Error(), http.StatusConflict)
		return
	}
	act(s.cmd)
	fmt.Fprintf(w, "sent: %s", name)
}

// setTemp handles the parameterized set-bed-temp / set-nozzle-temp endpoints:
// parse and range-check the temp, apply the idle guard, then send it.
func (s *Server) setTemp(w http.ResponseWriter, r *http.Request, connected bool, gs, name string, max int, set, record func(int)) {
	temp, err := parseTemp(r.URL.Query().Get("temp"), max)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if guardErr := p1s.ActionAllowed(connected, gs); guardErr != nil {
		http.Error(w, "blocked: "+guardErr.Error(), http.StatusConflict)
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
	if guardErr := p1s.ActionAllowed(connected, gs); guardErr != nil {
		http.Error(w, "blocked: "+guardErr.Error(), http.StatusConflict)
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

func (s *Server) camera(w http.ResponseWriter, r *http.Request) {
	frames, detach := s.hub.Attach()
	defer detach()
	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=frame")
	rc := http.NewResponseController(w)
	for {
		select {
		case <-r.Context().Done():
			return
		case frame := <-frames:
			fmt.Fprintf(w, "--frame\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", len(frame))
			w.Write(frame)
			fmt.Fprint(w, "\r\n")
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}
