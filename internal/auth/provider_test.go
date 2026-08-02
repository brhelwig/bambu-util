package auth

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

// provider is a stand-in OpenID Connect provider: discovery, a key set, an
// authorization endpoint and a token endpoint. It exists so the tests exercise
// the real verification — signatures checked against a published key set — the
// way a live provider would, rather than stubbing that out and proving nothing.
type provider struct {
	*httptest.Server
	key *rsa.PrivateKey

	mu        sync.Mutex
	nonce     string // what the login asked to be echoed back
	challenge string // the PKCE challenge the login sent
	verifier  string // the verifier the token exchange presented

	// Levers the tests pull to make the provider misbehave.
	signWith  *rsa.PrivateKey // a different key than the one published
	sendNonce string          // a nonce other than the one asked for
	subject   string
	name      string
}

func newProvider(t *testing.T) *provider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	p := &provider{key: key, subject: "user-1", name: "Ada"}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                 p.URL,
			"authorization_endpoint": p.URL + "/authorize",
			"token_endpoint":         p.URL + "/token",
			"jwks_uri":               p.URL + "/jwks",
			"end_session_endpoint":   p.URL + "/logout",
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, _ *http.Request) {
		pub := p.key.Public().(*rsa.PublicKey)
		writeJSON(w, map[string]any{"keys": []map[string]any{{
			"kty": "RSA",
			"kid": "test",
			"alg": "RS256",
			"use": "sig",
			"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		}}})
	})
	// The browser lands here, and the provider sends it back with a code.
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		p.mu.Lock()
		p.nonce = q.Get("nonce")
		p.challenge = q.Get("code_challenge")
		p.mu.Unlock()
		back, _ := url.Parse(q.Get("redirect_uri"))
		got := back.Query()
		got.Set("code", "the-code")
		got.Set("state", q.Get("state"))
		back.RawQuery = got.Encode()
		http.Redirect(w, r, back.String(), http.StatusFound)
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		p.mu.Lock()
		p.verifier = r.Form.Get("code_verifier")
		nonce := p.nonce
		if p.sendNonce != "" {
			nonce = p.sendNonce
		}
		signWith := p.key
		if p.signWith != nil {
			signWith = p.signWith
		}
		p.mu.Unlock()

		writeJSON(w, map[string]any{
			"access_token": "an-access-token",
			"token_type":   "Bearer",
			"id_token": p.idToken(signWith, map[string]any{
				"iss":   p.URL,
				"sub":   p.subject,
				"aud":   "the-client",
				"exp":   time.Now().Add(time.Hour).Unix(),
				"iat":   time.Now().Unix(),
				"nonce": nonce,
				"name":  p.name,
			}),
		})
	})
	mux.HandleFunc("GET /logout", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	p.Server = httptest.NewServer(mux)
	t.Cleanup(p.Close)
	return p
}

// idToken signs claims as a JWT. Hand-rolled rather than pulled from a library,
// because a signer the tests own is the only way to sign with the wrong key on
// purpose.
func (p *provider) idToken(key *rsa.PrivateKey, claims map[string]any) string {
	part := func(v any) string {
		raw, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	signing := part(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "test"}) + "." + part(claims)
	sum := sha256.Sum256([]byte(signing))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		panic(err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(signature)
}

// challengeFor is the PKCE transformation, so a test can check the verifier
// presented at the token endpoint really answers the challenge sent at login.
func challengeFor(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (p *provider) seen() (nonce, challenge, verifier string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.nonce, p.challenge, p.verifier
}
