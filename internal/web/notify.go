package web

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/brhelwig/bambu-util/internal/push"
)

// subscribeRequest is what a browser's PushSubscription serializes to.
type subscribeRequest struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

func (s *Server) pushKey(w http.ResponseWriter, _ *http.Request) {
	count, err := s.notify.Count()
	if err != nil {
		http.Error(w, "cannot read subscriptions", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"key": s.notify.PublicKey(), "subscribed": count})
}

func (s *Server) pushSubscribe(w http.ResponseWriter, r *http.Request) {
	var req subscribeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "invalid subscription", http.StatusBadRequest)
		return
	}
	sub, err := parseSubscription(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.notify.Subscribe(sub, s.now().Unix()); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func parseSubscription(req subscribeRequest) (push.Subscription, error) {
	p256dh, err := base64.RawURLEncoding.DecodeString(req.Keys.P256dh)
	if err != nil {
		return push.Subscription{}, fmt.Errorf("subscription key is not base64url")
	}
	auth, err := base64.RawURLEncoding.DecodeString(req.Keys.Auth)
	if err != nil {
		return push.Subscription{}, fmt.Errorf("auth secret is not base64url")
	}
	return push.Subscription{Endpoint: req.Endpoint, P256dh: p256dh, Auth: auth}, nil
}

func (s *Server) pushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Endpoint string `json:"endpoint"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.Endpoint == "" {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if err := s.notify.Unsubscribe(req.Endpoint); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// pushTest proves the path — server, push service, phone — without waiting on
// the printer.
func (s *Server) pushTest(w http.ResponseWriter, r *http.Request) {
	delivered, err := s.notify.Send(r.Context(), push.Notification{
		Title: "Bambu Util",
		Body:  "Notifications are working.",
		Tag:   "test",
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"delivered": delivered})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// preferencesRequest is what the settings screen sends when a device changes
// what it wants to hear about.
type preferencesRequest struct {
	Endpoint    string   `json:"endpoint"`
	Kinds       []string `json:"kinds"`
	BedInterval int      `json:"bedInterval"` // seconds; 0 is never
}

// pushPreferences reports what one device asked for. The endpoint identifies
// the device, and only that device knows its own.
func (s *Server) pushPreferences(w http.ResponseWriter, r *http.Request) {
	endpoint := r.URL.Query().Get("endpoint")
	if endpoint == "" {
		http.Error(w, "no endpoint", http.StatusBadRequest)
		return
	}
	sub, ok, err := s.notify.Preferences(endpoint)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "not subscribed", http.StatusNotFound)
		return
	}
	// A device that has chosen nothing is told about everything, so report the
	// full set rather than an empty one it would show as all-off.
	kinds := sub.Kinds
	if len(kinds) == 0 {
		kinds = push.Kinds
	}
	writeJSON(w, map[string]any{
		"available":   push.Kinds,
		"kinds":       kinds,
		"bedInterval": int(sub.BedInterval.Seconds()),
	})
}

func (s *Server) setPushPreferences(w http.ResponseWriter, r *http.Request) {
	var req preferencesRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.Endpoint == "" {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if req.Kinds == nil {
		req.Kinds = []string{}
	}
	if err := s.notify.SetPreferences(req.Endpoint, req.Kinds, time.Duration(req.BedInterval)*time.Second); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
