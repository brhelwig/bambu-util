package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Config describes the OIDC provider. Every field is required.
type Config struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	// PublicURL is where the app is reached from a browser. It is configured
	// rather than taken from request headers, which the client controls.
	PublicURL string
}

// CallbackPath is where the provider sends the browser back. The redirect URI
// registered with the provider is PublicURL with this on the end.
const CallbackPath = "/auth/callback"

// discoveryTimeout bounds how long startup waits on the provider.
const discoveryTimeout = 30 * time.Second

// extendEvery is how stale a session's expiry may get before a request
// extends it, so a polling page doesn't write on every request.
const extendEvery = time.Hour

// loginWindow is how long a started login may take to finish.
const loginWindow = 15 * time.Minute

// cookieName is the session cookie. It holds only the session id.
const cookieName = "bambu_session"

// stateCookieName carries the state of a login that is part-way through, so the
// callback can tell it is finishing the login this browser started.
const stateCookieName = "bambu_login"

// Authenticator answers whether a request may proceed, and runs the login.
type Authenticator struct {
	store    *Store
	verifier *oidc.IDTokenVerifier
	oauth    oauth2.Config
	// public is PublicURL parsed. Logins run on its host, and its scheme
	// decides whether cookies are Secure.
	public *url.URL
	// endSession is the provider's logout URL, if it advertises one.
	endSession string
	sessionFor func() time.Duration
	now        func() time.Time
}

// New reads the provider's discovery document and returns an authenticator, so
// a wrong issuer fails at startup.
func New(ctx context.Context, cfg Config, store *Store, sessionFor func() time.Duration) (*Authenticator, error) {
	public, err := url.Parse(strings.TrimRight(cfg.PublicURL, "/"))
	if err != nil || public.Host == "" {
		return nil, fmt.Errorf("%s=%q is not a URL with a host", EnvPublicURL, cfg.PublicURL)
	}
	// The provider's key set does not keep this context, so the timeout only
	// covers discovery.
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

// openPaths are served without a login: the health check and the files a
// phone fetches before it can log in.
var openPaths = map[string]bool{
	"/healthz":              true,
	"/sw.js":                true,
	"/manifest.webmanifest": true,
	"/icon-192.png":         true,
	"/icon-512.png":         true,
}

// Handler puts the login in front of next. A nil authenticator means login is
// disabled; the cross-origin check still applies.
func (a *Authenticator) Handler(next http.Handler) http.Handler {
	if a == nil {
		// Without this, any web page open on the LAN could post actions here.
		return http.NewCrossOriginProtection().Handler(next)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/login", a.login)
	mux.HandleFunc("GET "+CallbackPath, a.callback)
	mux.HandleFunc("POST /auth/logout", a.logout)
	mux.HandleFunc("GET /auth/me", a.me)
	mux.Handle("/", a.guard(next))

	// SameSite=Lax doesn't stop a sibling subdomain (same site), so also refuse
	// unsafe requests the browser says came from another origin.
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
		// Sliding expiry: extend the session and re-issue the cookie (which the
		// browser would otherwise drop at its old expiry), at most once per
		// extendEvery.
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

// refuse turns away a request with no session: pages are redirected to log
// in, API and camera requests get a 401.
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
	// The callback lands on PublicURL, so the state cookie must be set on that
	// host. Move a login started on another address (e.g. a LAN IP) there first.
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
	// Bind the login to this browser, so an attacker can't start a login and
	// have a victim complete it (login CSRF).
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

// safeNext keeps the post-login redirect inside this app. Browsers treat a
// backslash as a slash, so "/\evil.example" means "//evil.example"; reject
// backslashes, a leading "//", and anything with a scheme or host.
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
		// The provider refused; don't loop back to it.
		http.Error(w, "the login provider refused: "+reason, http.StatusForbidden)
		return
	}
	state := r.URL.Query().Get("state")
	// Must be the browser that started this login.
	started, err := r.Cookie(stateCookieName)
	if err != nil || started.Value == "" || started.Value != state {
		http.Error(w, "that login was not started here, start again", http.StatusBadRequest)
		return
	}
	// Single use.
	http.SetCookie(w, &http.Cookie{
		Name: stateCookieName, Value: "", Path: "/auth",
		Expires: time.Unix(0, 0), HttpOnly: true,
		Secure: a.secure(), SameSite: http.SameSiteLaxMode,
	})

	pending, err := a.store.TakeLogin(state, a.now())
	if err != nil {
		// Unknown or already used.
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
	// Checks the signature, audience and expiry.
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

	// Return where to go instead of redirecting: the page calls this with
	// fetch, which can't follow a cross-origin redirect to the provider.
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

// writeJSON writes v as a JSON response.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("auth: writing a response: %v", err)
	}
}

// cookie builds the session cookie.
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

// secure is whether cookies are marked Secure. It follows PublicURL rather
// than request headers, which a proxy may not set.
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
