package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Config is what the app is told about its provider. Every field is required:
// a half-configured login is refused at startup rather than discovered by
// someone finding the printer unguarded.
type Config struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	// PublicURL is where the app is reached from a browser. It is given rather
	// than worked out from the request, because a redirect built out of a header
	// the client controls is how people end up sending their login somewhere
	// else — and the provider has to be told the exact URL in any case.
	PublicURL string
}

// CallbackPath is where the provider sends the browser back. The redirect URI
// registered with the provider is PublicURL with this on the end.
const CallbackPath = "/auth/callback"

// discoveryTimeout bounds how long startup waits on the provider. A provider
// that is down or silently dropping packets would otherwise hold the app at
// startup forever, saying nothing and never answering its health check.
const discoveryTimeout = 30 * time.Second

// extendEvery is how stale a session's expiry may get before using the app
// pushes it out again. Extending on every request would write to the database
// several times a second for each open page, which polls; once an hour is
// indistinguishable to someone who logs in every few days.
const extendEvery = time.Hour

// loginWindow is how long a part-way login may sit unfinished. Long enough to
// find a passkey, short enough that abandoned ones do not pile up.
const loginWindow = 15 * time.Minute

// cookieName is the session cookie. It carries the session's id and nothing
// else — everything about who is logged in is in the database.
const cookieName = "bambu_session"

// stateCookieName carries the state of a login that is part-way through, so the
// callback can tell it is finishing the login this browser started.
const stateCookieName = "bambu_login"

// Authenticator answers whether a request may proceed, and runs the login.
type Authenticator struct {
	store    *Store
	verifier *oidc.IDTokenVerifier
	oauth    oauth2.Config
	// public is PublicURL parsed. Its host is the one every login runs on, and
	// its scheme decides whether cookies are marked Secure.
	public *url.URL
	// endSession is the provider's logout URL when it advertises one, so
	// logging out here does not leave a provider session that logs straight
	// back in on the next click.
	endSession string
	sessionFor func() time.Duration
	now        func() time.Time
}

// New reads the provider's discovery document and returns an authenticator.
// Discovery happens here rather than on the first request, so a wrong issuer
// stops the app at startup instead of when someone tries to log in.
func New(ctx context.Context, cfg Config, store *Store, sessionFor func() time.Duration) (*Authenticator, error) {
	public, err := url.Parse(strings.TrimRight(cfg.PublicURL, "/"))
	if err != nil || public.Host == "" {
		return nil, fmt.Errorf("%s=%q is not a URL with a host", EnvPublicURL, cfg.PublicURL)
	}
	// The key set the provider hands back keeps none of this context's
	// cancellation, so the deadline covers discovery and nothing after it.
	discover, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()
	provider, err := oidc.NewProvider(discover, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("read the provider's configuration at %s: %w", cfg.Issuer, err)
	}
	var extra struct {
		EndSession string `json:"end_session_endpoint"`
	}
	if err := provider.Claims(&extra); err != nil {
		return nil, fmt.Errorf("read the provider's configuration: %w", err)
	}
	return &Authenticator{
		store:    store,
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		oauth: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Endpoint:     provider.Endpoint(),
			RedirectURL:  public.String() + CallbackPath,
			Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
		},
		public:     public,
		endSession: extra.EndSession,
		sessionFor: sessionFor,
		now:        time.Now,
	}, nil
}

// openPaths are served without a login. The health check, so a container does
// not need credentials to be told the app is up; and the few files a phone
// fetches before it can log in — none of which says anything about the printer.
var openPaths = map[string]bool{
	"/healthz":              true,
	"/sw.js":                true,
	"/manifest.webmanifest": true,
	"/icon-192.png":         true,
	"/icon-512.png":         true,
}

// Handler puts the login in front of next. A nil authenticator means the app
// was started with authentication switched off, and everything goes straight
// through — apart from the cross-origin check, which applies either way.
func (a *Authenticator) Handler(next http.Handler) http.Handler {
	if a == nil {
		// With no login there is no cookie to borrow, but there is no need for
		// one: a page open in any browser on the network could post an action
		// to the printer's LAN address and it would be carried out.
		return http.NewCrossOriginProtection().Handler(next)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/login", a.login)
	mux.HandleFunc("GET "+CallbackPath, a.callback)
	mux.HandleFunc("POST /auth/logout", a.logout)
	mux.HandleFunc("GET /auth/me", a.me)
	mux.Handle("/", a.guard(next))

	// SameSite=Lax keeps the cookie off requests from other sites, but a
	// sibling subdomain counts as the same site, so anything served elsewhere
	// under the same domain could still post an action with it attached. This
	// refuses any unsafe request the browser says came from another origin.
	protect := http.NewCrossOriginProtection()
	if err := protect.AddTrustedOrigin(a.public.Scheme + "://" + a.public.Host); err != nil {
		log.Printf("auth: trusting %s as an origin: %v", a.public, err)
	}
	return protect.Handler(mux)
}

// guard lets a request through only when it carries a live session.
func (a *Authenticator) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if openPaths[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		session, err := a.session(r)
		if err != nil {
			a.refuse(w, r)
			return
		}
		// Using the app keeps you logged in; the clock only runs out on a
		// browser that has stopped coming back. The cookie is re-issued as well
		// as the row, because a browser throws its cookie away at the expiry it
		// was given and would never come back to be extended again. Both wait
		// until the expiry is extendEvery stale, so a polling page is not a
		// stream of writes.
		until := a.now().Add(a.sessionFor())
		if session.Expires.Before(until.Add(-extendEvery)) {
			if err := a.store.Extend(session.ID, until); err != nil {
				log.Printf("auth: extending a session: %v", err)
			} else {
				http.SetCookie(w, a.cookie(session.ID, until))
			}
		}
		next.ServeHTTP(w, r)
	})
}

// session returns the live session the request carries, if any.
func (a *Authenticator) session(r *http.Request) (*Session, error) {
	cookie, err := r.Cookie(cookieName)
	if err != nil {
		return nil, ErrNoSession
	}
	return a.store.Session(cookie.Value, a.now())
}

// refuse turns away a request with no session. What it sends depends on what
// asked: a page is sent to log in, but the page's own fetches are told plainly,
// because a fetch that follows a redirect and parses a login page as JSON fails
// in a way nobody can read.
func (a *Authenticator) refuse(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/camera/") {
		http.Error(w, "not logged in", http.StatusUnauthorized)
		return
	}
	to := "/auth/login"
	if next := r.URL.RequestURI(); next != "/" {
		to += "?next=" + url.QueryEscape(next)
	}
	http.Redirect(w, r, to, http.StatusFound)
}

func (a *Authenticator) login(w http.ResponseWriter, r *http.Request) {
	// The provider sends the browser back to PublicURL, so the state cookie has
	// to be set on that host too; set anywhere else, the callback arrives
	// without it and is refused. A login begun on another name for the app — a
	// LAN address, say — is moved over before anything is set.
	if !strings.EqualFold(r.Host, a.public.Host) {
		http.Redirect(w, r, a.public.String()+r.URL.RequestURI(), http.StatusFound)
		return
	}
	state, err := token()
	if err != nil {
		http.Error(w, "could not start the login", http.StatusInternalServerError)
		return
	}
	nonce, err := token()
	if err != nil {
		http.Error(w, "could not start the login", http.StatusInternalServerError)
		return
	}
	verifier := oauth2.GenerateVerifier()

	if err := a.store.StartLogin(state, verifier, nonce,
		safeNext(r.URL.Query().Get("next")), a.now().Add(loginWindow)); err != nil {
		log.Printf("auth: starting a login: %v", err)
		http.Error(w, "could not start the login", http.StatusInternalServerError)
		return
	}
	// The state is also handed to the browser, so the callback can tell that the
	// login it is finishing is the one this browser started. Without it, someone
	// could start a login themselves and walk a victim through the callback,
	// leaving that victim's browser holding a session that is not theirs.
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookieName,
		Value:    state,
		Path:     "/auth",
		Expires:  a.now().Add(loginWindow),
		HttpOnly: true,
		Secure:   a.secure(),
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, a.oauth.AuthCodeURL(state,
		oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), http.StatusFound)
}

// safeNext keeps where a login returns to inside this app.
//
// Anything else makes the login an open redirect, and the test cannot simply be
// "starts with a slash": browsers resolve a URL by the WHATWG rules, where a
// backslash counts as a separator just like a slash, so "/\elsewhere.example"
// reads to a browser as "//elsewhere.example" — another site. Go's own parser
// escapes the backslash instead, so agreeing with Go here would not be enough.
// Hence: one leading slash, no second separator of either kind, no backslash
// anywhere, and nothing that parses as having a scheme or a host.
func safeNext(next string) string {
	const home = "/"
	if !strings.HasPrefix(next, "/") || strings.Contains(next, `\`) {
		return home
	}
	if strings.HasPrefix(next, "//") {
		return home
	}
	parsed, err := url.Parse(next)
	if err != nil || parsed.Scheme != "" || parsed.Host != "" {
		return home
	}
	out := parsed.EscapedPath()
	if parsed.RawQuery != "" {
		out += "?" + parsed.RawQuery
	}
	if !strings.HasPrefix(out, "/") {
		return home
	}
	return out
}

func (a *Authenticator) callback(w http.ResponseWriter, r *http.Request) {
	if reason := r.URL.Query().Get("error"); reason != "" {
		// The provider turned them away — say so rather than looping back to it.
		http.Error(w, "the login provider refused: "+reason, http.StatusForbidden)
		return
	}
	state := r.URL.Query().Get("state")
	// The browser has to show that it is the one that started this login. A
	// callback walked through by somebody else arrives without the cookie.
	started, err := r.Cookie(stateCookieName)
	if err != nil || started.Value == "" || started.Value != state {
		http.Error(w, "that login was not started here, start again", http.StatusBadRequest)
		return
	}
	// Spent either way now, so it cannot be presented a second time.
	http.SetCookie(w, &http.Cookie{
		Name: stateCookieName, Value: "", Path: "/auth",
		Expires: time.Unix(0, 0), HttpOnly: true,
		Secure: a.secure(), SameSite: http.SameSiteLaxMode,
	})

	pending, err := a.store.TakeLogin(state, a.now())
	if err != nil {
		// Either nothing is waiting on this state or it has already been used.
		// Both mean the same thing here: do not trust it.
		http.Error(w, "that login has expired, start again", http.StatusBadRequest)
		return
	}

	oauthToken, err := a.oauth.Exchange(r.Context(), r.URL.Query().Get("code"),
		oauth2.VerifierOption(pending.Verifier))
	if err != nil {
		log.Printf("auth: exchanging the code: %v", err)
		http.Error(w, "the login could not be completed", http.StatusBadGateway)
		return
	}
	rawID, ok := oauthToken.Extra("id_token").(string)
	if !ok {
		http.Error(w, "the provider returned no identity token", http.StatusBadGateway)
		return
	}
	// This is the check the whole thing rests on: the signature against the
	// provider's published keys, the audience, and the expiry.
	idToken, err := a.verifier.Verify(r.Context(), rawID)
	if err != nil {
		log.Printf("auth: verifying the identity token: %v", err)
		http.Error(w, "the identity token could not be verified", http.StatusForbidden)
		return
	}
	if idToken.Nonce != pending.Nonce {
		http.Error(w, "the identity token belongs to a different login", http.StatusForbidden)
		return
	}

	var claims struct {
		Name              string `json:"name"`
		PreferredUsername string `json:"preferred_username"`
		Email             string `json:"email"`
	}
	if err := idToken.Claims(&claims); err != nil {
		log.Printf("auth: reading the identity token's claims: %v", err)
	}
	name := claims.Name
	for _, alternative := range []string{claims.PreferredUsername, claims.Email, idToken.Subject} {
		if name != "" {
			break
		}
		name = alternative
	}

	now := a.now()
	id, err := a.store.CreateSession(idToken.Subject, name, now, now.Add(a.sessionFor()))
	if err != nil {
		log.Printf("auth: opening a session: %v", err)
		http.Error(w, "the login could not be completed", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, a.cookie(id, now.Add(a.sessionFor())))
	log.Printf("auth: %s logged in", name)
	http.Redirect(w, r, pending.Next, http.StatusFound)
}

func (a *Authenticator) logout(w http.ResponseWriter, r *http.Request) {
	if session, err := a.session(r); err == nil {
		if err := a.store.EndSession(session.ID); err != nil {
			log.Printf("auth: ending a session: %v", err)
		}
	}
	// An expiry in the past is how a cookie is taken back.
	http.SetCookie(w, a.cookie("", time.Unix(0, 0)))

	// Where to go next is reported rather than redirected to. A redirect here
	// would be followed by the page's own fetch, and the provider's end-session
	// endpoint answers no cross-origin request, so the browser would refuse it
	// and the page would sit there looking logged in.
	to := "/"
	if a.endSession != "" {
		to = a.endSession
	}
	writeJSON(w, map[string]string{"then": to})
}

// me reports who is logged in, so the page can show it and offer to log out.
func (a *Authenticator) me(w http.ResponseWriter, r *http.Request) {
	session, err := a.session(r)
	if err != nil {
		http.Error(w, "not logged in", http.StatusUnauthorized)
		return
	}
	writeJSON(w, map[string]string{"name": session.Name, "subject": session.Subject})
}

// writeJSON is how every answer here is written: encoded rather than assembled,
// so a display name carrying anything unusual cannot produce a body the page
// then fails to read.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("auth: writing a response: %v", err)
	}
}

// cookie builds the session cookie.
//
// SameSite=Lax keeps it off requests that start on another site. It does not
// stop a sibling subdomain, which is the same site; the cross-origin check in
// Handler covers that.
func (a *Authenticator) cookie(value string, expires time.Time) *http.Cookie {
	return &http.Cookie{
		Name:     cookieName,
		Value:    value,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   a.secure(),
		SameSite: http.SameSiteLaxMode,
	}
}

// secure is whether cookies are marked Secure. It follows PublicURL rather than
// how each request arrived: a header saying the request came over HTTPS is the
// client's to set, and a proxy may not set it at all. Marking a cookie Secure
// on a plain-HTTP deployment would mean the browser never sends it back.
func (a *Authenticator) secure() bool {
	return a.public.Scheme == "https"
}

// RunSweeper clears out lapsed sessions and abandoned logins on every tick of
// interval until ctx is cancelled. Call once, from main.
func RunSweeper(ctx context.Context, store *Store, interval time.Duration, now func() time.Time) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := store.Sweep(now()); err != nil {
				log.Printf("auth: sweeping lapsed sessions: %v", err)
			}
		}
	}
}

// ErrNotConfigured is returned when the app was given neither a provider nor
// permission to run without one.
var ErrNotConfigured = errors.New("auth: nothing was decided about authentication")
