package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// guarded is an app behind the login: a stand-in for everything the real server
// serves, so the tests are about the guard rather than about the printer.
func guarded() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("the page"))
	})
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"connected":false}`))
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /sw.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("// worker"))
	})
	return mux
}

// setup wires a stand-in provider to an app behind the login, and returns the
// app's address along with the pieces a test may need to meddle with.
func setup(t *testing.T) (*httptest.Server, *provider, *Authenticator, *Store) {
	t.Helper()
	p := newProvider(t)

	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	// The redirect URL has to be the app's own address, which is not known until
	// it is listening, so the handler is filled in once it is.
	var handler http.Handler
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(app.Close)

	a, err := New(context.Background(), Config{
		Issuer:       p.URL,
		ClientID:     "the-client",
		ClientSecret: "the-secret",
		PublicURL:    app.URL,
	}, store, func() time.Duration { return 14 * 24 * time.Hour })
	if err != nil {
		t.Fatalf("new authenticator: %v", err)
	}
	handler = a.Handler(guarded())
	return app, p, a, store
}

// client follows redirects and keeps cookies, which is what a browser does and
// what the whole flow depends on.
func client(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	return &http.Client{Jar: jar}
}

func get(t *testing.T, c *http.Client, url string) *http.Response {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// The whole round trip, the way a browser walks it.
func TestLoggingInAndReachingTheApp(t *testing.T) {
	app, p, _, _ := setup(t)
	c := client(t)

	resp := get(t, c, app.URL+"/auth/login")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("after logging in: %d, want 200", resp.StatusCode)
	}
	// Having come back through the provider, the app is now reachable.
	if resp := get(t, c, app.URL+"/api/status"); resp.StatusCode != http.StatusOK {
		t.Errorf("/api/status = %d after logging in, want 200", resp.StatusCode)
	}

	// PKCE really happened: the verifier presented at the token endpoint is the
	// answer to the challenge sent at login.
	_, challenge, verifier := p.seen()
	if challenge == "" || verifier == "" {
		t.Fatalf("no PKCE exchange happened: challenge=%q verifier=%q", challenge, verifier)
	}
	if challengeFor(verifier) != challenge {
		t.Error("the verifier presented does not answer the challenge sent")
	}
}

func TestWithoutALoginThePageIsSentToTheProviderAndTheApiIsRefused(t *testing.T) {
	app, _, _, _ := setup(t)
	// A client that stops rather than following, so the redirect can be read.
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	resp, err := c.Get(app.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("/ = %d with no login, want a redirect", resp.StatusCode)
	}
	if to := resp.Header.Get("Location"); !strings.HasPrefix(to, "/auth/login") {
		t.Errorf("/ redirected to %q, want the login", to)
	}

	// The page's own fetches must be told plainly, not sent HTML to parse.
	api, err := c.Get(app.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer api.Body.Close()
	if api.StatusCode != http.StatusUnauthorized {
		t.Errorf("/api/status = %d with no login, want 401", api.StatusCode)
	}
}

// A deep link has to survive the trip to the provider and back.
func TestALoginReturnsToWhereItStarted(t *testing.T) {
	app, _, _, _ := setup(t)
	c := client(t)

	var landed string
	c.CheckRedirect = func(r *http.Request, _ []*http.Request) error {
		landed = r.URL.Path
		return nil
	}
	get(t, c, app.URL+"/auth/login?next="+url.QueryEscape("/api/status"))
	if landed != "/api/status" {
		t.Errorf("ended at %q, want the page the login started from", landed)
	}
}

// Sending someone to another site after logging in would make this an open
// redirect. Browsers resolve a URL by the WHATWG rules, where a backslash
// separates just as a slash does, so the backslash forms have to go the same
// way as the obvious ones.
func TestALoginWillNotSendYouToAnotherSite(t *testing.T) {
	away := []string{
		"https://evil.example.com/",
		"//evil.example.com/",
		`/\evil.example.com/`,
		`/\/evil.example.com/`,
		`\\evil.example.com/`,
		"http:/evil.example.com",
	}
	for _, next := range away {
		if got := safeNext(next); got != "/" {
			t.Errorf("a login for %q would return to %q, want the app's own root", next, got)
		}
	}
	// And somewhere inside the app is still allowed through untouched.
	for _, next := range []string{"/", "/api/status", "/camera/history/frame?ts=5"} {
		if got := safeNext(next); got != next {
			t.Errorf("safeNext(%q) = %q, want it kept", next, got)
		}
	}
}

// The whole path, not just the helper: what is stored is what the callback
// will send the browser to.
func TestABackslashDestinationIsNotStored(t *testing.T) {
	app, _, _, store := setup(t)
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := c.Get(app.URL + `/auth/login?next=%2F%5Cevil.example.com%2F`)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	to, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	pending, err := store.TakeLogin(to.Query().Get("state"), time.Now())
	if err != nil {
		t.Fatalf("no login was recorded: %v", err)
	}
	if pending.Next != "/" {
		t.Errorf("stored %q as where to return to, want the app's own root", pending.Next)
	}
}

// The check the whole thing rests on. Anyone can mint a token; only the
// provider can sign one.
func TestATokenSignedWithTheWrongKeyIsRefused(t *testing.T) {
	app, p, _, _ := setup(t)
	other := newProvider(t) // a different key, not the one published
	p.signWith = other.key

	c := client(t)
	resp := get(t, c, app.URL+"/auth/login")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d, want 403 for a token signed by the wrong key", resp.StatusCode)
	}
	if resp := get(t, c, app.URL+"/api/status"); resp.StatusCode != http.StatusUnauthorized {
		t.Error("a session was opened despite the token being unverifiable")
	}
}

// The nonce ties the token to this particular login.
func TestATokenForADifferentLoginIsRefused(t *testing.T) {
	app, p, _, _ := setup(t)
	p.sendNonce = "some-other-login"

	resp := get(t, client(t), app.URL+"/auth/login")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status %d, want 403 when the nonce does not match", resp.StatusCode)
	}
}

func TestACallbackWithAnUnknownStateIsRefused(t *testing.T) {
	app, _, _, _ := setup(t)
	resp := get(t, client(t), app.URL+CallbackPath+"?code=x&state=never-issued")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d, want 400 for a state nothing is waiting on", resp.StatusCode)
	}
}

// Once used, a state is spent: replaying the callback must not open a second
// session.
func TestACallbackCannotBeReplayed(t *testing.T) {
	app, _, _, _ := setup(t)
	c := client(t)

	var callback string
	c.CheckRedirect = func(r *http.Request, _ []*http.Request) error {
		if strings.HasPrefix(r.URL.Path, CallbackPath) {
			callback = r.URL.String()
		}
		return nil
	}
	get(t, c, app.URL+"/auth/login")
	if callback == "" {
		t.Fatal("the flow never reached the callback")
	}

	resp := get(t, client(t), callback)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("replaying the callback gave %d, want 400", resp.StatusCode)
	}
}

// A provider that turns someone away should say so, not bounce them round the
// loop again.
func TestAProviderRefusalIsReported(t *testing.T) {
	app, _, _, _ := setup(t)
	resp := get(t, client(t), app.URL+CallbackPath+"?error=access_denied")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status %d, want 403 when the provider refused", resp.StatusCode)
	}
}

func TestTheOpenPathsNeedNoLogin(t *testing.T) {
	app, _, _, _ := setup(t)
	for _, path := range []string{"/healthz", "/sw.js"} {
		if resp := get(t, client(t), app.URL+path); resp.StatusCode != http.StatusOK {
			t.Errorf("%s = %d with no login, want 200", path, resp.StatusCode)
		}
	}
}

func TestALapsedSessionIsRefused(t *testing.T) {
	app, _, a, _ := setup(t)
	c := client(t)
	get(t, c, app.URL+"/auth/login")

	// Far enough ahead that the session, and any extension of it, has run out.
	a.now = func() time.Time { return time.Now().Add(365 * 24 * time.Hour) }
	if resp := get(t, c, app.URL+"/api/status"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("/api/status = %d with a lapsed session, want 401", resp.StatusCode)
	}
}

func TestLoggingOutEndsTheSessionAtOnce(t *testing.T) {
	app, _, _, _ := setup(t)
	c := client(t)
	get(t, c, app.URL+"/auth/login")

	resp, err := c.Post(app.URL+"/auth/logout", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if resp := get(t, c, app.URL+"/api/status"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("/api/status = %d after logging out, want 401", resp.StatusCode)
	}
}

// Using the app keeps you logged in.
func TestUsingTheAppExtendsTheSession(t *testing.T) {
	app, _, a, store := setup(t)
	c := client(t)
	get(t, c, app.URL+"/auth/login")

	var id string
	if err := store.db.QueryRow(`SELECT id FROM sessions`).Scan(&id); err != nil {
		t.Fatalf("no session was opened: %v", err)
	}
	before, err := store.Session(id, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	a.now = func() time.Time { return time.Now().Add(24 * time.Hour) }
	get(t, c, app.URL+"/api/status")

	after, err := store.Session(id, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !after.Expires.After(before.Expires) {
		t.Errorf("session still lapses at %s, want it pushed out from %s", after.Expires, before.Expires)
	}
}

// The cookie is the credential, so its flags are the difference between a
// session another site can borrow and one it cannot.
func TestTheSessionCookieIsLockedDown(t *testing.T) {
	app, _, _, _ := setup(t)
	c := client(t)

	// The cookie is set part-way through the redirect chain, and a jar keeps
	// only name and value, so every hop's headers are read on the way past.
	var found *http.Cookie
	c.CheckRedirect = func(r *http.Request, _ []*http.Request) error {
		for _, cookie := range r.Response.Cookies() {
			if cookie.Name == cookieName && cookie.Value != "" {
				found = cookie
			}
		}
		return nil
	}
	get(t, c, app.URL+"/auth/login")

	if found == nil {
		t.Fatal("no session cookie was set anywhere in the login")
	}
	if !found.HttpOnly {
		t.Error("the session cookie is readable by scripts")
	}
	if found.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax so another site cannot post with it", found.SameSite)
	}
	if found.Path != "/" {
		t.Errorf("path = %q, want /", found.Path)
	}
}

// With no authenticator at all the app is served as it was before any of this
// existed, which is what the off switch has to mean.
func TestWithNoAuthenticatorEverythingIsServed(t *testing.T) {
	var none *Authenticator
	app := httptest.NewServer(none.Handler(guarded()))
	t.Cleanup(app.Close)

	for _, path := range []string{"/", "/api/status", "/healthz"} {
		if resp := get(t, client(t), app.URL+path); resp.StatusCode != http.StatusOK {
			t.Errorf("%s = %d with authentication off, want 200", path, resp.StatusCode)
		}
	}
}

// Extending the row is not enough: a browser drops its cookie at the expiry it
// was given, so a session that is only extended in the database still logs a
// daily user out on the day it was first opened.
func TestUsingTheAppReissuesTheCookie(t *testing.T) {
	app, _, a, _ := setup(t)
	c := client(t)
	get(t, c, app.URL+"/auth/login")

	later := time.Now().Add(24 * time.Hour)
	a.now = func() time.Time { return later }

	resp := get(t, c, app.URL+"/api/status")
	var reissued *http.Cookie
	for _, cookie := range resp.Cookies() {
		if cookie.Name == cookieName {
			reissued = cookie
		}
	}
	if reissued == nil {
		t.Fatal("using the app did not re-issue the session cookie, so the browser still drops it on the original schedule")
	}
	if !reissued.Expires.After(later) {
		t.Errorf("the re-issued cookie expires at %s, want it pushed past %s", reissued.Expires, later)
	}
}

// A login someone else started must not be completable in this browser.
func TestACallbackFromABrowserThatDidNotStartTheLoginIsRefused(t *testing.T) {
	app, _, _, _ := setup(t)

	// One browser starts a login and gets as far as the callback URL.
	starter := client(t)
	var callback string
	starter.CheckRedirect = func(r *http.Request, _ []*http.Request) error {
		if strings.HasPrefix(r.URL.Path, CallbackPath) {
			callback = r.URL.String()
			return http.ErrUseLastResponse
		}
		return nil
	}
	get(t, starter, app.URL+"/auth/login")
	if callback == "" {
		t.Fatal("the flow never reached the callback")
	}

	// A different browser — one that never started it — presents the callback.
	victim := client(t)
	resp := get(t, victim, callback)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d, want 400 for a login this browser did not start", resp.StatusCode)
	}
	if resp := get(t, victim, app.URL+"/api/status"); resp.StatusCode != http.StatusUnauthorized {
		t.Error("a session was opened for a browser that did not start the login")
	}
}

// Logging out reports where to go rather than redirecting, because a fetch
// cannot follow a redirect to a provider that answers no cross-origin request.
func TestLoggingOutReportsWhereToGoNext(t *testing.T) {
	app, p, _, _ := setup(t)
	c := client(t)
	get(t, c, app.URL+"/auth/login")

	resp, err := c.Post(app.URL+"/auth/logout", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200 with somewhere to go", resp.StatusCode)
	}
	var got struct {
		Then string `json:"then"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// This provider advertises one, so that is where the page is sent.
	if got.Then != p.URL+"/logout" {
		t.Errorf("then = %q, want the provider's own log-out page", got.Then)
	}
}

// A name the provider sends must not be able to produce a body the page cannot
// read — which is what building JSON by hand allowed.
func TestWhoIsLoggedInIsRealJSON(t *testing.T) {
	app, p, _, _ := setup(t)
	p.name = "Ada \"Lovelace\"\x7f \U0001f600"

	c := client(t)
	get(t, c, app.URL+"/auth/login")

	resp := get(t, c, app.URL+"/auth/me")
	var who struct {
		Name    string `json:"name"`
		Subject string `json:"subject"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&who); err != nil {
		t.Fatalf("the page could not read who is logged in: %v", err)
	}
	if who.Name != p.name {
		t.Errorf("name = %q, want %q", who.Name, p.name)
	}
}

// Starting a login needs no login, so the table it writes to has to be bounded
// or anything that can reach the port could grow the database — and the size
// cap would answer by deleting camera footage instead.
func TestPartWayLoginsAreBounded(t *testing.T) {
	app, _, _, store := setup(t)
	for range MaxPendingLogins + 50 {
		c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}
		resp, err := c.Get(app.URL + "/auth/login")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	var held int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM pending_logins`).Scan(&held); err != nil {
		t.Fatal(err)
	}
	if held > MaxPendingLogins {
		t.Errorf("holding %d part-way logins, over the %d bound", held, MaxPendingLogins)
	}
}
