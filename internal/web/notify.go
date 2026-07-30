package web

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/brhelwig/bambu-util/internal/push"
)

// subscribeRequest is the shape a browser's PushSubscription serializes to.
type subscribeRequest struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// pushKey hands the browser the identity it must subscribe against.
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

// The browser sends its keys base64url-encoded without padding, the same form
// the Push API hands out.
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

// pushTest proves the whole path — this server, the push service, the phone —
// without waiting for the printer to do something.
func (s *Server) pushTest(w http.ResponseWriter, r *http.Request) {
	delivered, err := s.notify.Send(r.Context(), push.Notification{
		Title: "P1S Bridge",
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
