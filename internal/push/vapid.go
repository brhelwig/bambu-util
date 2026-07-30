package push

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"time"
)

// tokenLifetime is how long a signed authorization token stays valid. RFC 8292
// caps this at 24 hours; a token is minted per delivery, so this only has to
// outlive one request.
const tokenLifetime = 12 * time.Hour

// Contact is the address a push service can complain to about this
// application server, as required by RFC 8292. It is never shown to the user.
const Contact = "mailto:brandon@helwig.me"

// Key is the application server's identity. Browsers bind a subscription to the
// public half, so replacing this key invalidates every existing subscription.
type Key struct {
	priv *ecdsa.PrivateKey
}

// NewKey generates a fresh application server identity.
func NewKey() (*Key, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	return &Key{priv: priv}, nil
}

// ParseKey restores a key from the form Marshal produces.
func ParseKey(der []byte) (*Key, error) {
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, err
	}
	priv, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || priv.Curve != elliptic.P256() {
		return nil, fmt.Errorf("push: stored key is not a P-256 key")
	}
	return &Key{priv: priv}, nil
}

// Marshal renders the key for storage.
func (k *Key) Marshal() ([]byte, error) { return x509.MarshalPKCS8PrivateKey(k.priv) }

// Public returns the key the browser needs when subscribing, in the base64url
// form the Push API expects.
func (k *Key) Public() string {
	ecdhKey, err := k.priv.ECDH()
	if err != nil {
		// Unreachable: the curve is checked wherever a key enters.
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(ecdhKey.PublicKey().Bytes())
}

// authorization builds the header that proves this message came from the holder
// of the key the subscription was created with. audience is the push service's
// own origin, not the subscription path — the token authorizes the server, not
// one delivery.
func (k *Key) authorization(endpoint string, now time.Time) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("push: endpoint %q: %w", endpoint, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("push: endpoint %q has no origin", endpoint)
	}
	claims := map[string]any{
		"aud": u.Scheme + "://" + u.Host,
		"exp": now.Add(tokenLifetime).Unix(),
		"sub": Contact,
	}
	token, err := k.sign(claims)
	if err != nil {
		return "", err
	}
	return "vapid t=" + token + ", k=" + k.Public(), nil
}

func (k *Key) sign(claims map[string]any) (string, error) {
	body, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	// The header is fixed, so it is written out rather than marshalled.
	signing := enc.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`)) + "." + enc.EncodeToString(body)

	digest := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, k.priv, digest[:])
	if err != nil {
		return "", err
	}
	// The signature travels as the two values padded to the curve's size, not
	// as the ASN.1 structure ecdsa.SignASN1 would produce.
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signing + "." + enc.EncodeToString(sig), nil
}
