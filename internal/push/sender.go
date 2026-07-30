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

	"github.com/brhelwig/bambu-util/internal/activity"
)

// deliveryTTL is how long a push service holds a message for a phone that is
// off. Long enough to survive a night's sleep, short enough that nothing
// arrives a day stale.
const deliveryTTL = 4 * time.Hour

// The kinds of notification a device can ask for. They are stored against each
// subscription, so the names outlive any one release and must not be reworded
// casually.
const (
	KindPrintStarted  = "print-started"
	KindPrintFinished = "print-finished"
	KindPrintEnded    = "print-ended"
	KindPrinterError  = "printer-error"
	KindHeaterOff     = "heater-off"
	KindBedReminder   = "bed-reminder"
)

// Kinds is every kind a device can choose between. The bed reminder is not
// here: it is not on or off but an interval, kept on the subscription itself.
var Kinds = []string{
	KindPrintStarted,
	KindPrintFinished,
	KindPrintEnded,
	KindPrinterError,
	KindHeaterOff,
}

// Notification is what shows up on the phone. A second one carrying the same
// Tag replaces the first rather than stacking, so repeated reminders about one
// condition stay a single line.
type Notification struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Tag   string `json:"tag,omitempty"`
	// Kind decides which devices are told. It stays on this side of the wire.
	Kind string `json:"-"`
}

// Sender delivers notifications to every subscribed browser.
type Sender struct {
	store  *Store
	key    *Key
	client *http.Client
	log    *activity.Log
	now    func() time.Time
}

// Watch records what is sent out, so a notification can be seen leaving
// alongside the printer traffic that prompted it.
func (s *Sender) Watch(log *activity.Log) { s.log = log }

// NewSender loads the server identity, creating one on first use.
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

// Send delivers to every subscription and reports how many were reached. Only
// a subscription the push service calls gone is forgotten: a timeout or a
// server error is temporary, and the phone behind it is still real.
func (s *Sender) Send(ctx context.Context, n Notification) (delivered int, err error) {
	subs, err := s.store.All()
	if err != nil {
		return 0, err
	}
	payload, err := json.Marshal(n)
	if err != nil {
		return 0, err
	}
	wanted := 0
	for _, sub := range subs {
		if n.Kind != "" && !sub.Wants(n.Kind) {
			continue
		}
		wanted++
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
	entry := s.log.Record(activity.Notification,
		fmt.Sprintf("%s → %d of %d devices", n.Title, delivered, wanted), n.Body)
	if delivered < wanted {
		s.log.Acknowledge(entry, time.Time{}, fmt.Errorf("%d not delivered", wanted-delivered))
	} else {
		s.log.Acknowledge(entry, s.now(), nil)
	}
	return delivered, nil
}

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
	// Push services explain a rejection in the body.
	detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))

	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		return true, nil
	}
	if resp.StatusCode >= 300 {
		return false, fmt.Errorf("push service returned %s: %s", resp.Status, bytes.TrimSpace(detail))
	}
	return false, nil
}

// RemindBedOn tells each device the bed is still on, as often as that device
// asked to hear it. The schedule is per subscription rather than shared: two
// devices asking for different intervals must each get their own, and a device
// that has never asked hears nothing.
//
// since is when the bed came on, so the reminder can say how long it has been
// rather than merely that it is.
func (s *Sender) RemindBedOn(ctx context.Context, since time.Time, target float64) error {
	subs, err := s.store.All()
	if err != nil {
		return err
	}
	now := s.now()
	for _, sub := range subs {
		if sub.BedInterval <= 0 {
			continue
		}
		// A device that has not been reminded during this stretch is due one
		// interval after the bed came on, not immediately.
		last := sub.BedRemindedAt
		if last.IsZero() {
			last = since
		}
		if now.Sub(last) < sub.BedInterval {
			continue
		}
		n := Notification{
			Title: fmt.Sprintf("Bed on for %s", roundedHours(now.Sub(since))),
			Body:  fmt.Sprintf("Holding %.0f°C.", target),
			Tag:   "bed",
			Kind:  KindBedReminder,
		}
		payload, err := json.Marshal(n)
		if err != nil {
			return err
		}
		gone, err := s.deliver(ctx, sub, payload)
		switch {
		case gone:
			if err := s.store.Delete(sub.Endpoint); err != nil {
				log.Printf("push: forgetting dead subscription: %v", err)
			}
			continue
		case err != nil:
			log.Printf("push: delivery failed: %v", err)
			continue
		}
		if err := s.store.MarkBedReminded(sub.Endpoint, now); err != nil {
			log.Printf("push: recording a reminder: %v", err)
		}
	}
	return nil
}

// ForgetBedReminders starts every device's reminder schedule over, for when the
// bed goes off or a print starts.
func (s *Sender) ForgetBedReminders() error { return s.store.ClearBedReminders() }

// Preferences reports what one device asked to be told about.
func (s *Sender) Preferences(endpoint string) (Subscription, bool, error) {
	return s.store.Find(endpoint)
}

// SetPreferences records what one device wants.
func (s *Sender) SetPreferences(endpoint string, kinds []string, bedInterval time.Duration) error {
	return s.store.SetPreferences(endpoint, kinds, bedInterval)
}

// roundedHours reports a stretch of time in whole hours, so a reminder that
// arrives a tick late still reads as the round number a person expects.
func roundedHours(d time.Duration) string {
	h := int(d.Round(time.Hour).Hours())
	if h <= 1 {
		return "1 hour"
	}
	return fmt.Sprintf("%d hours", h)
}
