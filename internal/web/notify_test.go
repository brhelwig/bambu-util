package web

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/brhelwig/bambu-util/internal/p1s"
	"github.com/brhelwig/bambu-util/internal/push"
)

// notifyTestServer returns a running server plus the notifier behind it, so a
// test can check what actually landed in the subscription store.
func notifyTestServer(t *testing.T) (*httptest.Server, *push.Sender) {
	t.Helper()
	cache := p1s.NewStateCache()
	cache.SetConnected(true)
	notifier := openTestNotifier()
	ts := httptest.NewServer(NewServer(cache, &fakeCommander{}, openTestStore(), notifier, nil, testSettings, nil).Handler())
	t.Cleanup(ts.Close)
	return ts, notifier
}

// browserSubscription builds what a browser posts when the user turns
// notifications on: base64url values, exactly as the Push API hands them out.
func browserSubscription(t *testing.T, endpoint string) map[string]any {
	t.Helper()
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	auth := make([]byte, 16)
	if _, err := rand.Read(auth); err != nil {
		t.Fatalf("generate auth: %v", err)
	}
	return map[string]any{
		"endpoint": endpoint,
		"keys": map[string]any{
			"p256dh": base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes()),
			"auth":   base64.RawURLEncoding.EncodeToString(auth),
		},
	}
}

func postJSON(t *testing.T, ts *httptest.Server, path string, body any) *http.Response {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp, err := ts.Client().Post(ts.URL+path, "application/json", strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("post %s: %v", path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestPushKeyIsServedAndUsable(t *testing.T) {
	ts, notifier := notifyTestServer(t)
	resp, err := ts.Client().Get(ts.URL + "/api/push/key")
	if err != nil {
		t.Fatalf("get key: %v", err)
	}
	defer resp.Body.Close()
	var got struct {
		Key        string `json:"key"`
		Subscribed int    `json:"subscribed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Key != notifier.PublicKey() {
		t.Errorf("key = %q, want %q", got.Key, notifier.PublicKey())
	}
	// The browser passes this straight to subscribe(), which rejects anything
	// that is not a point on the curve.
	raw, err := base64.RawURLEncoding.DecodeString(got.Key)
	if err != nil {
		t.Fatalf("key is not base64url: %v", err)
	}
	if _, err := ecdh.P256().NewPublicKey(raw); err != nil {
		t.Errorf("key is not usable as an application server key: %v", err)
	}
	if got.Subscribed != 0 {
		t.Errorf("subscribed = %d, want 0", got.Subscribed)
	}
}

func TestSubscribeThenUnsubscribe(t *testing.T) {
	ts, notifier := notifyTestServer(t)
	sub := browserSubscription(t, "https://push.example.net/abc")

	if resp := postJSON(t, ts, "/api/push/subscribe", sub); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("subscribe returned %d, want 204", resp.StatusCode)
	}
	if n, err := notifier.Count(); err != nil || n != 1 {
		t.Fatalf("stored %d subscriptions (err %v), want 1", n, err)
	}

	// Turning it on twice — a reload, a second tap — must not double up.
	if resp := postJSON(t, ts, "/api/push/subscribe", sub); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("second subscribe returned %d, want 204", resp.StatusCode)
	}
	if n, _ := notifier.Count(); n != 1 {
		t.Errorf("stored %d subscriptions after re-subscribing, want 1", n)
	}

	body := map[string]any{"endpoint": sub["endpoint"]}
	if resp := postJSON(t, ts, "/api/push/unsubscribe", body); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("unsubscribe returned %d, want 204", resp.StatusCode)
	}
	if n, _ := notifier.Count(); n != 0 {
		t.Errorf("stored %d subscriptions after unsubscribing, want 0", n)
	}
}

func TestSubscribeRefusesSomethingUnusable(t *testing.T) {
	ts, notifier := notifyTestServer(t)
	good := browserSubscription(t, "https://push.example.net/abc")
	shortKey := base64.RawURLEncoding.EncodeToString([]byte("too short"))

	cases := map[string]any{
		"not json":        "{",
		"no endpoint":     map[string]any{"endpoint": "", "keys": good["keys"]},
		"key not base64":  map[string]any{"endpoint": "https://p/a", "keys": map[string]any{"p256dh": "!!!", "auth": "AAAA"}},
		"key wrong size":  map[string]any{"endpoint": "https://p/a", "keys": map[string]any{"p256dh": shortKey, "auth": "AAAA"}},
		"no auth secret":  map[string]any{"endpoint": "https://p/a", "keys": map[string]any{"p256dh": good["keys"].(map[string]any)["p256dh"], "auth": ""}},
		"nothing at all":  map[string]any{},
		"keys omitted":    map[string]any{"endpoint": "https://p/a"},
		"auth not base64": map[string]any{"endpoint": "https://p/a", "keys": map[string]any{"p256dh": good["keys"].(map[string]any)["p256dh"], "auth": "!!!"}},
	}
	for name, body := range cases {
		resp := postJSON(t, ts, "/api/push/subscribe", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, resp.StatusCode)
		}
	}
	if n, _ := notifier.Count(); n != 0 {
		t.Errorf("stored %d subscriptions, want 0", n)
	}
}

func TestUnsubscribeRefusesAnEmptyRequest(t *testing.T) {
	ts, _ := notifyTestServer(t)
	for name, body := range map[string]any{
		"no endpoint": map[string]any{},
		"empty":       map[string]any{"endpoint": ""},
	} {
		if resp := postJSON(t, ts, "/api/push/unsubscribe", body); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, resp.StatusCode)
		}
	}
}

func TestTestNotificationReachesTheSubscribedDevice(t *testing.T) {
	var mu sync.Mutex
	var got int
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		mu.Lock()
		got++
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	defer svc.Close()

	ts, _ := notifyTestServer(t)
	for i := range 2 {
		sub := browserSubscription(t, fmt.Sprintf("%s/push/%d", svc.URL, i))
		if resp := postJSON(t, ts, "/api/push/subscribe", sub); resp.StatusCode != http.StatusNoContent {
			t.Fatalf("subscribe returned %d", resp.StatusCode)
		}
	}

	resp := postJSON(t, ts, "/api/push/test", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("test returned %d, want 200", resp.StatusCode)
	}
	var out struct {
		Delivered int `json:"delivered"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Delivered != 2 {
		t.Errorf("delivered = %d, want 2", out.Delivered)
	}
	mu.Lock()
	defer mu.Unlock()
	if got != 2 {
		t.Errorf("push service saw %d messages, want 2", got)
	}
}

// The service worker is what receives a push, and it only has authority over
// the paths under it. Served from anywhere but the root it would not cover the
// page.
func TestServiceWorkerIsServedFromTheRoot(t *testing.T) {
	ts, _ := notifyTestServer(t)
	resp, err := ts.Client().Get(ts.URL + "/sw.js")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `addEventListener("push"`) {
		t.Error("the file served is not the notification worker")
	}
}

func TestIconsAreServedForTheHomeScreen(t *testing.T) {
	ts, _ := notifyTestServer(t)
	resp, err := ts.Client().Get(ts.URL + "/manifest.webmanifest")
	if err != nil {
		t.Fatalf("get manifest: %v", err)
	}
	defer resp.Body.Close()
	var manifest struct {
		Icons []struct {
			Src   string `json:"src"`
			Sizes string `json:"sizes"`
		} `json:"icons"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&manifest); err != nil {
		t.Fatalf("manifest is not JSON: %v", err)
	}
	if len(manifest.Icons) == 0 {
		t.Fatal("manifest declares no icons, so the Home Screen entry has no artwork")
	}
	for _, icon := range manifest.Icons {
		r, err := ts.Client().Get(ts.URL + icon.Src)
		if err != nil {
			t.Fatalf("get %s: %v", icon.Src, err)
		}
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Errorf("%s: status %d", icon.Src, r.StatusCode)
			continue
		}
		if !strings.HasPrefix(string(body), "\x89PNG") {
			t.Errorf("%s (%s) is not a PNG", icon.Src, icon.Sizes)
		}
	}
}
