package push

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"
)

// deliveryTTL is how long a push service holds a message for a phone that is
// off or out of range. Four hours keeps overnight news — a print that finished
// while you slept — without delivering something a day stale.
const deliveryTTL = 4 * time.Hour

// Notification is what shows up on the phone. Tag groups messages: a second
// notification with the same tag replaces the first rather than stacking, so
// repeated reminders about the same condition stay one line.
type Notification struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Tag   string `json:"tag,omitempty"`
}

// Sender delivers notifications to every stored subscription.
type Sender struct {
	store  *Store
	key    *Key
	client *http.Client
	now    func() time.Time
}

// NewSender loads (or creates) the server identity and returns a sender.
func NewSender(store *Store) (*Sender, error) {
	key, err := store.Key()
	if err != nil {
		return nil, err
	}
	return &Sender{
		store:  store,
		key:    key,
		client: &http.Client{Timeout: 15 * time.Second},
		now:    time.Now,
	}, nil
}

// PublicKey is the value a browser needs to subscribe to this server.
func (s *Sender) PublicKey() string { return s.key.Public() }

// Subscribe records a browser's subscription.
func (s *Sender) Subscribe(sub Subscription, ts int64) error { return s.store.Save(sub, ts) }

// Unsubscribe forgets one, when the user turns notifications off.
func (s *Sender) Unsubscribe(endpoint string) error { return s.store.Delete(endpoint) }

// Count reports how many browsers are subscribed.
func (s *Sender) Count() (int, error) { return s.store.Count() }

// Send delivers one notification to every subscription and reports how many
// were reached. Subscriptions the push service says are gone are deleted;
// everything else is left alone, since a timeout or a server error is
// temporary and the phone behind it is still real.
func (s *Sender) Send(ctx context.Context, n Notification) (delivered int, err error) {
	subs, err := s.store.All()
	if err != nil {
		return 0, err
	}
	payload, err := json.Marshal(n)
	if err != nil {
		return 0, err
	}
	for _, sub := range subs {
		gone, err := s.deliver(ctx, sub, payload)
		switch {
		case gone:
			if err := s.store.Delete(sub.Endpoint); err != nil {
				log.Printf("push: forgetting dead subscription: %v", err)
			}
		case err != nil:
			log.Printf("push: delivery failed: %v", err)
		default:
			delivered++
		}
	}
	return delivered, nil
}

// deliver posts one message. gone reports that the push service says this
// subscription no longer exists, which is the only case where forgetting it is
// the right response.
func (s *Sender) deliver(ctx context.Context, sub Subscription, payload []byte) (gone bool, err error) {
	body, err := encryptRandom(sub.P256dh, sub.Auth, payload)
	if err != nil {
		return false, fmt.Errorf("encrypt for %s: %w", sub.Endpoint, err)
	}
	auth, err := s.key.authorization(sub.Endpoint, s.now())
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", strconv.Itoa(int(deliveryTTL.Seconds())))

	resp, err := s.client.Do(req)
	if err != nil {
		return false, fmt.Errorf("post to %s: %w", sub.Endpoint, err)
	}
	defer resp.Body.Close()
	// Read enough of the response to report why a rejection happened; push
	// services explain themselves in the body.
	detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))

	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		return true, nil
	}
	if resp.StatusCode >= 300 {
		return false, fmt.Errorf("push service returned %s: %s", resp.Status, bytes.TrimSpace(detail))
	}
	return false, nil
}
