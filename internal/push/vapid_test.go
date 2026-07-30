package push

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

// verify checks a token against the key that signed it and returns its claims.
// Delivery is one-way, so nothing outside a test ever needs this.
func verify(t *testing.T, k *Key, token string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts, want 3", len(parts))
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	if len(sig) != 64 {
		t.Fatalf("signature is %d bytes, want the two 32-byte halves", len(sig))
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(&k.priv.PublicKey, digest[:], r, s) {
		t.Fatal("signature does not verify against the signing key")
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(body, &claims); err != nil {
		t.Fatalf("claims are not JSON: %v", err)
	}
	return claims
}

// token pulls the signed part out of the header value.
func token(t *testing.T, header string) string {
	t.Helper()
	rest, ok := strings.CutPrefix(header, "vapid t=")
	if !ok {
		t.Fatalf("header does not start with the scheme name: %q", header)
	}
	tok, _, ok := strings.Cut(rest, ", k=")
	if !ok {
		t.Fatalf("header carries no public key: %q", header)
	}
	return tok
}

func TestAuthorizationIsSignedAndNamesThePushService(t *testing.T) {
	k, err := NewKey()
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	header, err := k.authorization("https://push.example.net/push/JzLQ3raZJfFBR0aq?x=1", testNow)
	if err != nil {
		t.Fatalf("authorization: %v", err)
	}
	if !strings.HasSuffix(header, ", k="+k.Public()) {
		t.Errorf("header does not carry this key's public half: %q", header)
	}

	claims := verify(t, k, token(t, header))
	// The token authorizes the server to the push service, so it names the
	// service's origin — carrying the subscription path would leak it.
	if claims["aud"] != "https://push.example.net" {
		t.Errorf("audience = %v, want the push service origin with no path", claims["aud"])
	}
	if claims["sub"] != Contact {
		t.Errorf("contact = %v, want %v", claims["sub"], Contact)
	}
	exp, ok := claims["exp"].(float64)
	if !ok {
		t.Fatalf("expiry missing or not a number: %v", claims["exp"])
	}
	if want := testNow.Add(tokenLifetime).Unix(); int64(exp) != want {
		t.Errorf("expiry = %d, want %d", int64(exp), want)
	}
	// RFC 8292 refuses a token valid for more than 24 hours.
	if int64(exp)-testNow.Unix() > int64((24 * time.Hour).Seconds()) {
		t.Errorf("token is valid for longer than 24 hours")
	}
}

func TestAuthorizationRejectsAnEndpointWithNoOrigin(t *testing.T) {
	k, err := NewKey()
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	for _, endpoint := range []string{"", "/push/abc", "not a url"} {
		if _, err := k.authorization(endpoint, testNow); err == nil {
			t.Errorf("endpoint %q was accepted", endpoint)
		}
	}
}

func TestAKeySurvivesBeingStoredAndRead(t *testing.T) {
	k, err := NewKey()
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	der, err := k.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	restored, err := ParseKey(der)
	if err != nil {
		t.Fatalf("ParseKey: %v", err)
	}
	// A browser binds its subscription to this value. If it changed across a
	// restart every phone would silently stop receiving.
	if restored.Public() != k.Public() {
		t.Errorf("public key changed across storage:\n got %s\nwant %s", restored.Public(), k.Public())
	}

	// A token signed by the restored key must verify against the original.
	header, err := restored.authorization("https://push.example.net/p", testNow)
	if err != nil {
		t.Fatalf("authorization: %v", err)
	}
	verify(t, k, token(t, header))
}

func TestPublicKeyIsAnUncompressedPoint(t *testing.T) {
	k, err := NewKey()
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(k.Public())
	if err != nil {
		t.Fatalf("public key is not base64url: %v", err)
	}
	if len(raw) != keyLength {
		t.Errorf("public key is %d bytes, want %d", len(raw), keyLength)
	}
	if raw[0] != 4 {
		t.Errorf("public key does not start with the uncompressed-point marker: %#x", raw[0])
	}
}

func TestParseKeyRejectsSomethingThatIsNotAKey(t *testing.T) {
	if _, err := ParseKey([]byte("not a key")); err == nil {
		t.Error("arbitrary bytes were accepted as a key")
	}
}
