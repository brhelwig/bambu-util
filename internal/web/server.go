package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"

	"github.com/brhelwig/bambu-util/internal/p1s"
)

//go:embed static
var staticFS embed.FS

// Commander is what the HTTP layer needs from the printer link.
type Commander interface {
	LowerBed()
	Home()
	SetBedTemp(int)
	PausePrint()
	ResumePrint()
	StopPrint()
}

type Server struct {
	cache *p1s.StateCache
	cmd   Commander
	hub   *Hub
}

func NewServer(cache *p1s.StateCache, cmd Commander, hub *Hub) *Server {
	return &Server{cache: cache, cmd: cmd, hub: hub}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /", http.FileServerFS(static))
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
	resp := map[string]any{
		"connected":      connected,
		"gcodeState":     gs,
		"actionsAllowed": p1s.ActionAllowed(connected, gs) == nil,
		"bedTemp":        fields["bed_temper"],
		"bedTarget":      fields["bed_target_temper"],
		"nozzleTemp":     fields["nozzle_temper"],
		"nozzleTarget":   fields["nozzle_target_temper"],
		"progress":       fields["mc_percent"],
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
	"bed-heat":  func(c Commander) { c.SetBedTemp(p1s.BedDryTemp) },
	"heat-off":  func(c Commander) { c.SetBedTemp(0) },
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
