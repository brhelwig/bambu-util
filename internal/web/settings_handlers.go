package web

import (
	"net/http"
	"strconv"

	"github.com/brhelwig/bambu-util/internal/settings"
)

// settingsWriter is the writing half of the settings store. Reading arrives
// separately as current, because almost everything here only reads.
type settingsWriter interface {
	Set(name string, value int) error
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
