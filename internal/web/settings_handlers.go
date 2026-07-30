package web

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/brhelwig/bambu-util/internal/p1s"
	"github.com/brhelwig/bambu-util/internal/settings"
)

// settingsWriter is the writing half of the settings store. Reading arrives
// separately as current, because almost everything here only reads.
type settingsWriter interface {
	Set(name string, value int) error
	SetText(name, value string) error
}

// getSettings reports the current values as whole numbers: seconds for a
// length of time, a plain count otherwise, leaving units to whatever displays
// them.
func (s *Server) getSettings(w http.ResponseWriter, _ *http.Request) {
	v := s.settings()
	writeJSON(w, map[string]any{
		settings.KeyRetention:      int(v.Retention.Seconds()),
		settings.KeyKeptJobs:       v.KeptJobs,
		settings.KeyBedOffAfter:    int(v.BedOffAfter.Seconds()),
		settings.KeyNozzleOffAfter: int(v.NozzleOffAfter.Seconds()),
		settings.KeyLampOffAfter:   int(v.LampOffAfter.Seconds()),
	})
}

// setSetting stores one value. Nothing has to be told about the change:
// everything that consults a setting does so at the point of use.
func (s *Server) setSetting(w http.ResponseWriter, r *http.Request) {
	if s.writeSettings == nil {
		http.Error(w, "settings are not writable", http.StatusNotImplemented)
		return
	}
	value, err := strconv.Atoi(r.URL.Query().Get("value"))
	if err != nil {
		http.Error(w, "invalid value", http.StatusBadRequest)
		return
	}
	// The store refuses an unknown name and an out-of-range value; both are the
	// caller's fault rather than the server's.
	if err := s.writeSettings.Set(r.PathValue("name"), value); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// printerConfigurer is what the HTTP layer needs to point the app at a printer.
type printerConfigurer interface {
	Config() p1s.Config
	Configure(p1s.Config)
}

// printerRequest is what the setup screen sends. An access code left out keeps
// whatever is stored, so the form can be re-saved without retyping a secret it
// was never shown.
type printerRequest struct {
	IP         string `json:"ip"`
	Serial     string `json:"serial"`
	AccessCode string `json:"accessCode"`
}

// getPrinter reports the configured printer. The access code is never sent
// back — only whether there is one — because the page has no authentication and
// a credential that is never served cannot be read off it.
func (s *Server) getPrinter(w http.ResponseWriter, _ *http.Request) {
	conf := s.printer.Config()
	writeJSON(w, map[string]any{
		"ip":            conf.IP,
		"serial":        conf.Serial,
		"accessCodeSet": conf.AccessCode != "",
		"configured":    conf.Complete(),
	})
}

func (s *Server) setPrinter(w http.ResponseWriter, r *http.Request) {
	if s.writeSettings == nil {
		http.Error(w, "settings are not writable", http.StatusNotImplemented)
		return
	}
	var req printerRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	req.IP = strings.TrimSpace(req.IP)
	req.Serial = strings.TrimSpace(req.Serial)
	req.AccessCode = strings.TrimSpace(req.AccessCode)

	code := req.AccessCode
	if code == "" {
		// Not sent means unchanged, not cleared: the form never receives the
		// stored code, so it cannot send it back.
		code = s.printer.Config().AccessCode
	}
	conf := p1s.Config{IP: req.IP, Serial: req.Serial, AccessCode: code}
	if !conf.Complete() {
		http.Error(w, "the printer's address, serial and access code are all needed", http.StatusBadRequest)
		return
	}

	for name, value := range map[string]string{
		settings.KeyPrinterIP:         conf.IP,
		settings.KeyPrinterSerial:     conf.Serial,
		settings.KeyPrinterAccessCode: conf.AccessCode,
	} {
		if err := s.writeSettings.SetText(name, value); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	// Saving is not the same as reaching it. Connecting happens in the
	// background and the connection row reports how it went; refusing to store
	// an address that does not answer would make a printer that is merely
	// switched off impossible to set up.
	s.printer.Configure(conf)
	w.WriteHeader(http.StatusNoContent)
}

// events reports what has recently gone to the printer, come back from it, or
// been sent to a phone — newest first, because that is what is being looked
// for.
func (s *Server) getEvents(w http.ResponseWriter, _ *http.Request) {
	entries := s.activity.Entries()
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	writeJSON(w, map[string]any{"events": entries})
}
