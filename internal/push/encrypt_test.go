package push

import (
	"bytes"
	"crypto/ecdh"
	"encoding/base64"
	"strings"
	"testing"
)

func b64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return b
}

// The worked example from RFC 8291 section 5. Both key pairs and the salt are
// fixed there, so the whole message is reproducible byte for byte — the only
// way to know the encryption is right rather than merely self-consistent.
const (
	rfcPlaintext    = "When I grow up, I want to be a watermelon"
	rfcUAPublic     = "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"
	rfcAuthSecret   = "BTBZMqHH6r4Tts7J_aSIgg"
	rfcASPrivate    = "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"
	rfcASPublic     = "BP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8"
	rfcSalt         = "DGv6ra1nlYgDCS1FRnbzlw"
	rfcSharedSecret = "kyrL1jIIOHEzg3sM2ZWRHDRB62YACZhhSlknJ672kSs"
	rfcCEK          = "oIhVW04MRdy2XN9CiKLxTg"
	rfcNonce        = "4h_95klXJ5E_qnoN"
	rfcBody         = "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27ml" +
		"mlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPT" +
		"pK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN"
)

func rfcASKey(t *testing.T) *ecdh.PrivateKey {
	t.Helper()
	key, err := ecdh.P256().NewPrivateKey(b64(t, rfcASPrivate))
	if err != nil {
		t.Fatalf("application server key: %v", err)
	}
	if got := base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()); got != rfcASPublic {
		t.Fatalf("public key derived from the RFC's private key = %s, want %s", got, rfcASPublic)
	}
	return key
}

func TestEncryptMatchesTheSpecificationsWorkedExample(t *testing.T) {
	got, err := encrypt(b64(t, rfcUAPublic), b64(t, rfcAuthSecret), []byte(rfcPlaintext), rfcASKey(t), b64(t, rfcSalt))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	want := b64(t, rfcBody)
	if !bytes.Equal(got, want) {
		t.Errorf("encrypted message does not match RFC 8291 section 5\n got %s\nwant %s",
			base64.RawURLEncoding.EncodeToString(got), rfcBody)
	}
}

func TestDeriveKeysMatchesTheSpecificationsIntermediateValues(t *testing.T) {
	uaKey, err := ecdh.P256().NewPublicKey(b64(t, rfcUAPublic))
	if err != nil {
		t.Fatalf("subscription key: %v", err)
	}
	asKey := rfcASKey(t)
	shared, err := asKey.ECDH(uaKey)
	if err != nil {
		t.Fatalf("key agreement: %v", err)
	}
	if got := base64.RawURLEncoding.EncodeToString(shared); got != rfcSharedSecret {
		t.Errorf("agreed secret = %s, want %s", got, rfcSharedSecret)
	}
	cek, nonce, err := deriveKeys(shared, b64(t, rfcAuthSecret), uaKey.Bytes(), asKey.PublicKey().Bytes(), b64(t, rfcSalt))
	if err != nil {
		t.Fatalf("deriveKeys: %v", err)
	}
	if got := base64.RawURLEncoding.EncodeToString(cek); got != rfcCEK {
		t.Errorf("content encryption key = %s, want %s", got, rfcCEK)
	}
	if got := base64.RawURLEncoding.EncodeToString(nonce); got != rfcNonce {
		t.Errorf("nonce = %s, want %s", got, rfcNonce)
	}
}

// The header the browser reads back has to carry this message's public key and
// salt, or it cannot derive the same key.
func TestEncryptedMessageCarriesItsOwnSaltAndKey(t *testing.T) {
	body, err := encryptRandom(b64(t, rfcUAPublic), b64(t, rfcAuthSecret), []byte(rfcPlaintext))
	if err != nil {
		t.Fatalf("encryptRandom: %v", err)
	}
	if len(body) < 16+4+1+keyLength {
		t.Fatalf("message too short to hold a header: %d bytes", len(body))
	}
	if got := int(body[20]); got != keyLength {
		t.Errorf("header says the key is %d bytes, want %d", got, keyLength)
	}
	if _, err := ecdh.P256().NewPublicKey(body[21 : 21+keyLength]); err != nil {
		t.Errorf("header does not carry a usable public key: %v", err)
	}
}

// Reusing a key pair or salt across messages would leak the plaintext, so the
// production path must not produce the same bytes twice.
func TestEachMessageGetsAFreshKeyAndSalt(t *testing.T) {
	first, err := encryptRandom(b64(t, rfcUAPublic), b64(t, rfcAuthSecret), []byte(rfcPlaintext))
	if err != nil {
		t.Fatalf("encryptRandom: %v", err)
	}
	second, err := encryptRandom(b64(t, rfcUAPublic), b64(t, rfcAuthSecret), []byte(rfcPlaintext))
	if err != nil {
		t.Fatalf("encryptRandom: %v", err)
	}
	if bytes.Equal(first[:16], second[:16]) {
		t.Error("two messages shared a salt")
	}
	if bytes.Equal(first[21:21+keyLength], second[21:21+keyLength]) {
		t.Error("two messages shared a key")
	}
}

func TestEncryptRejectsAKeyThatIsNotOnTheCurve(t *testing.T) {
	_, err := encryptRandom(bytes.Repeat([]byte{4}, keyLength), b64(t, rfcAuthSecret), []byte("x"))
	if err == nil {
		t.Fatal("a bogus subscription key was accepted")
	}
	if !strings.Contains(err.Error(), "subscription key") {
		t.Errorf("error does not say which input was bad: %v", err)
	}
}
