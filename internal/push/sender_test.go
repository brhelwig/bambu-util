package push

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// browser stands in for a subscribed phone: it holds the private half of the
// subscription and can therefore read what the server sends.
type browser struct {
	priv *ecdh.PrivateKey
	auth []byte
}

func newBrowser(t *testing.T) *browser {
	t.Helper()
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate subscription key: %v", err)
	}
	auth := make([]byte, 16)
	if _, err := rand.Read(auth); err != nil {
		t.Fatalf("generate auth secret: %v", err)
	}
	return &browser{priv: priv, auth: auth}
}

func (b *browser) subscription(endpoint string) Subscription {
	return Subscription{Endpoint: endpoint, P256dh: b.priv.PublicKey().Bytes(), Auth: b.auth}
}

// read undoes what the server did, the way a real browser would: pull this
// message's salt and public key out of the header, derive the same key, and
// decrypt.
func (b *browser) read(t *testing.T, body []byte) []byte {
	t.Helper()
	if len(body) < 16+4+1+keyLength {
		t.Fatalf("message is too short to hold a header: %d bytes", len(body))
	}
	salt := body[:16]
	if got := int(body[20]); got != keyLength {
		t.Fatalf("header says the key is %d bytes, want %d", got, keyLength)
	}
	asPublic, err := ecdh.P256().NewPublicKey(body[21 : 21+keyLength])
	if err != nil {
		t.Fatalf("header key: %v", err)
	}
	ciphertext := body[21+keyLength:]

	shared, err := b.priv.ECDH(asPublic)
	if err != nil {
		t.Fatalf("key agreement: %v", err)
	}
	cek, nonce, err := deriveKeys(shared, b.auth, b.priv.PublicKey().Bytes(), asPublic.Bytes(), salt)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("gcm: %v", err)
	}
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		t.Fatalf("the message did not decrypt: %v", err)
	}
	if len(plain) == 0 || plain[len(plain)-1] != 0x02 {
		t.Fatalf("message does not end with the last-record marker")
	}
	return plain[:len(plain)-1]
}

// pushService is a stand-in for Apple's or Google's endpoint. It records what
// arrived and answers with whatever status the test asks for.
type pushService struct {
	server *httptest.Server
	status int

	mu      sync.Mutex
	bodies  [][]byte
	headers []http.Header
}

func newPushService(t *testing.T) *pushService {
	t.Helper()
	p := &pushService{status: http.StatusCreated}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		p.bodies = append(p.bodies, body)
		p.headers = append(p.headers, r.Header.Clone())
		status := p.status
		p.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(p.server.Close)
	return p
}

func (p *pushService) endpoint() string { return p.server.URL + "/push/abc123" }

func (p *pushService) received() ([][]byte, []http.Header) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.bodies, p.headers
}

func TestSendDeliversAMessageTheBrowserCanRead(t *testing.T) {
	store := openTestStore(t)
	svc := newPushService(t)
	phone := newBrowser(t)
	if err := store.Save(phone.subscription(svc.endpoint()), testNow.Unix()); err != nil {
		t.Fatalf("save subscription: %v", err)
	}
	sender, err := NewSender(store)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}

	want := Notification{Title: "Print finished", Body: "benchy.gcode", Tag: "job"}
	delivered, err := sender.Send(context.Background(), want)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if delivered != 1 {
		t.Fatalf("delivered = %d, want 1", delivered)
	}

	bodies, headers := svc.received()
	if len(bodies) != 1 {
		t.Fatalf("push service saw %d messages, want 1", len(bodies))
	}

	var got Notification
	if err := json.Unmarshal(phone.read(t, bodies[0]), &got); err != nil {
		t.Fatalf("decrypted message is not a notification: %v", err)
	}
	if got != want {
		t.Errorf("notification = %+v, want %+v", got, want)
	}

	h := headers[0]
	if h.Get("Content-Encoding") != "aes128gcm" {
		t.Errorf("Content-Encoding = %q", h.Get("Content-Encoding"))
	}
	if h.Get("Content-Type") != "application/octet-stream" {
		t.Errorf("Content-Type = %q", h.Get("Content-Type"))
	}
	if ttl, err := strconv.Atoi(h.Get("TTL")); err != nil || ttl != int(deliveryTTL.Seconds()) {
		t.Errorf("TTL = %q, want %d seconds", h.Get("TTL"), int(deliveryTTL.Seconds()))
	}
	if auth := h.Get("Authorization"); !strings.HasPrefix(auth, "vapid t=") || !strings.Contains(auth, ", k="+sender.PublicKey()) {
		t.Errorf("Authorization = %q", auth)
	}
}

func TestSendForgetsASubscriptionThePushServiceSaysIsGone(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusGone} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			store := openTestStore(t)
			svc := newPushService(t)
			svc.status = status
			phone := newBrowser(t)
			if err := store.Save(phone.subscription(svc.endpoint()), testNow.Unix()); err != nil {
				t.Fatalf("save subscription: %v", err)
			}
			sender, err := NewSender(store)
			if err != nil {
				t.Fatalf("NewSender: %v", err)
			}
			delivered, err := sender.Send(context.Background(), Notification{Title: "x"})
			if err != nil {
				t.Fatalf("Send: %v", err)
			}
			if delivered != 0 {
				t.Errorf("delivered = %d, want 0", delivered)
			}
			if n, err := store.Count(); err != nil || n != 0 {
				t.Errorf("subscriptions left = %d (err %v), want 0", n, err)
			}
		})
	}
}

// A push service having a bad day is not a phone that has gone away. Dropping
// the subscription would silently switch notifications off for good.
func TestSendKeepsASubscriptionThroughAServerError(t *testing.T) {
	store := openTestStore(t)
	svc := newPushService(t)
	svc.status = http.StatusInternalServerError
	phone := newBrowser(t)
	if err := store.Save(phone.subscription(svc.endpoint()), testNow.Unix()); err != nil {
		t.Fatalf("save subscription: %v", err)
	}
	sender, err := NewSender(store)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	if _, err := sender.Send(context.Background(), Notification{Title: "x"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if n, err := store.Count(); err != nil || n != 1 {
		t.Errorf("subscriptions left = %d (err %v), want 1", n, err)
	}
}

func TestSendReachesEveryPhone(t *testing.T) {
	store := openTestStore(t)
	svc := newPushService(t)
	for i := range 3 {
		phone := newBrowser(t)
		sub := phone.subscription(svc.server.URL + "/push/" + strconv.Itoa(i))
		if err := store.Save(sub, testNow.Unix()); err != nil {
			t.Fatalf("save subscription: %v", err)
		}
	}
	sender, err := NewSender(store)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	delivered, err := sender.Send(context.Background(), Notification{Title: "x"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if delivered != 3 {
		t.Errorf("delivered = %d, want 3", delivered)
	}
}

func TestSendWithNoSubscriptionsDoesNothing(t *testing.T) {
	store := openTestStore(t)
	sender, err := NewSender(store)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	delivered, err := sender.Send(context.Background(), Notification{Title: "x"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if delivered != 0 {
		t.Errorf("delivered = %d, want 0", delivered)
	}
}

func TestSenderRespectsACancelledContext(t *testing.T) {
	store := openTestStore(t)
	svc := newPushService(t)
	phone := newBrowser(t)
	if err := store.Save(phone.subscription(svc.endpoint()), testNow.Unix()); err != nil {
		t.Fatalf("save subscription: %v", err)
	}
	sender, err := NewSender(store)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	delivered, err := sender.Send(ctx, Notification{Title: "x"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if delivered != 0 {
		t.Errorf("delivered = %d, want 0", delivered)
	}
	// The phone is fine; the request was abandoned at this end.
	if n, _ := store.Count(); n != 1 {
		t.Errorf("subscriptions left = %d, want 1", n)
	}
}

func TestSenderReusesTheStoredIdentityAcrossRestarts(t *testing.T) {
	path := t.TempDir() + "/push.db"
	first, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	a, err := NewSender(first)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	firstKey := a.PublicKey()
	first.Close()

	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()
	b, err := NewSender(second)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	// Every phone's subscription is bound to this value. If a restart changed
	// it, notifications would stop with nothing to show for it.
	if b.PublicKey() != firstKey {
		t.Errorf("identity changed across a restart:\n got %s\nwant %s", b.PublicKey(), firstKey)
	}
}

func subWithKinds(t *testing.T, store *Store, endpoint string, kinds []string, interval time.Duration) *browser {
	t.Helper()
	phone := newBrowser(t)
	if err := store.Save(phone.subscription(endpoint), testNow.Unix()); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := store.SetPreferences(endpoint, kinds, interval); err != nil {
		t.Fatalf("preferences: %v", err)
	}
	return phone
}

// Two devices should be able to want different things.
func TestOnlyTheDevicesThatAskedAreTold(t *testing.T) {
	store := openTestStore(t)
	svc := newPushService(t)
	subWithKinds(t, store, svc.server.URL+"/wants", []string{KindPrintFinished}, 0)
	subWithKinds(t, store, svc.server.URL+"/does-not", []string{KindPrinterError}, 0)

	sender, err := NewSender(store)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	delivered, err := sender.Send(context.Background(), Notification{Title: "Print finished", Kind: KindPrintFinished})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if delivered != 1 {
		t.Errorf("delivered to %d devices, want only the one that asked", delivered)
	}
}

// Turning notifications on and never opening the settings should mean being
// told about everything, not nothing.
func TestADeviceThatHasChosenNothingIsToldEverything(t *testing.T) {
	store := openTestStore(t)
	svc := newPushService(t)
	phone := newBrowser(t)
	if err := store.Save(phone.subscription(svc.endpoint()), testNow.Unix()); err != nil {
		t.Fatalf("save: %v", err)
	}
	sender, err := NewSender(store)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	for _, kind := range Kinds {
		delivered, err := sender.Send(context.Background(), Notification{Title: "x", Kind: kind})
		if err != nil {
			t.Fatalf("Send %s: %v", kind, err)
		}
		if delivered != 1 {
			t.Errorf("%s reached %d devices, want 1", kind, delivered)
		}
	}
}

// Re-subscribing happens on every page load. It must not undo what was chosen.
func TestResubscribingKeepsThePreferences(t *testing.T) {
	store := openTestStore(t)
	const endpoint = "https://push.example.net/a"
	phone := subWithKinds(t, store, endpoint, []string{KindPrinterError}, 4*time.Hour)

	if err := store.Save(phone.subscription(endpoint), testNow.Unix()+60); err != nil {
		t.Fatalf("re-save: %v", err)
	}
	sub, ok, err := store.Find(endpoint)
	if err != nil || !ok {
		t.Fatalf("Find: %v (found %v)", err, ok)
	}
	if len(sub.Kinds) != 1 || sub.Kinds[0] != KindPrinterError {
		t.Errorf("kinds = %v, want the chosen one kept", sub.Kinds)
	}
	if sub.BedInterval != 4*time.Hour {
		t.Errorf("interval = %s, want 4h kept", sub.BedInterval)
	}
}

func TestPreferencesRefuseAnUnknownKind(t *testing.T) {
	store := openTestStore(t)
	const endpoint = "https://push.example.net/a"
	subWithKinds(t, store, endpoint, []string{KindPrintFinished}, 0)
	if err := store.SetPreferences(endpoint, []string{"chamber-on-fire"}, 0); err == nil {
		t.Error("an unknown notification was accepted")
	}
	if err := store.SetPreferences("https://push.example.net/nobody", []string{KindPrintFinished}, 0); err == nil {
		t.Error("preferences were accepted for a device that is not subscribed")
	}
}

// Each device keeps its own place in its own schedule.
func TestBedRemindersFollowEachDevicesOwnInterval(t *testing.T) {
	store := openTestStore(t)
	svc := newPushService(t)
	subWithKinds(t, store, svc.server.URL+"/hourly", nil, time.Hour)
	subWithKinds(t, store, svc.server.URL+"/daily", nil, 24*time.Hour)
	subWithKinds(t, store, svc.server.URL+"/never", nil, 0)

	sender, err := NewSender(store)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	now := testNow
	sender.now = func() time.Time { return now }
	since := now

	// Nothing is due the moment the bed comes on.
	if err := sender.RemindBedOn(context.Background(), since, 60); err != nil {
		t.Fatalf("RemindBedOn: %v", err)
	}
	if bodies, _ := svc.received(); len(bodies) != 0 {
		t.Fatalf("reminded %d devices immediately, want none", len(bodies))
	}

	// An hour on, only the device that asked hourly hears.
	now = now.Add(time.Hour + time.Minute)
	if err := sender.RemindBedOn(context.Background(), since, 60); err != nil {
		t.Fatalf("RemindBedOn: %v", err)
	}
	bodies, _ := svc.received()
	if len(bodies) != 1 {
		t.Fatalf("reminded %d devices after an hour, want 1", len(bodies))
	}

	// And not again until its next hour is up.
	now = now.Add(30 * time.Minute)
	sender.RemindBedOn(context.Background(), since, 60)
	if bodies, _ := svc.received(); len(bodies) != 1 {
		t.Errorf("reminded again after 30 minutes: %d messages", len(bodies))
	}

	// A day on, both the hourly and the daily device hear.
	now = now.Add(24 * time.Hour)
	sender.RemindBedOn(context.Background(), since, 60)
	if bodies, _ := svc.received(); len(bodies) != 3 {
		t.Errorf("after a day there are %d messages, want 3 — the device asking never must stay silent", len(bodies))
	}
}

// The bed going off starts every device's schedule over.
func TestForgettingRemindersStartsTheScheduleAgain(t *testing.T) {
	store := openTestStore(t)
	svc := newPushService(t)
	subWithKinds(t, store, svc.endpoint(), nil, time.Hour)
	sender, err := NewSender(store)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	now := testNow
	sender.now = func() time.Time { return now }

	now = now.Add(2 * time.Hour)
	sender.RemindBedOn(context.Background(), testNow, 60)
	if bodies, _ := svc.received(); len(bodies) != 1 {
		t.Fatalf("got %d reminders, want 1", len(bodies))
	}

	if err := sender.ForgetBedReminders(); err != nil {
		t.Fatalf("ForgetBedReminders: %v", err)
	}
	sub, _, _ := store.Find(svc.endpoint())
	if !sub.BedRemindedAt.IsZero() {
		t.Errorf("a device still remembers being reminded: %v", sub.BedRemindedAt)
	}
}
