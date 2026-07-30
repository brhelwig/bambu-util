// Package push delivers Web Push notifications to subscribed browsers, using
// only the standard library: RFC 8291 message encryption over RFC 8188
// aes128gcm, authorized by an RFC 8292 signed token.
package push

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
)

const recordSize = 4096

// keyLength is an uncompressed P-256 point: what a browser hands over as its
// subscription key, and what the message header carries back.
const keyLength = 65

// encrypt builds one push message. asKey and salt are parameters rather than
// generated here so a test can reproduce the RFC's worked example exactly.
func encrypt(uaPublic, authSecret, plaintext []byte, asKey *ecdh.PrivateKey, salt []byte) ([]byte, error) {
	uaKey, err := ecdh.P256().NewPublicKey(uaPublic)
	if err != nil {
		return nil, fmt.Errorf("subscription key: %w", err)
	}
	shared, err := asKey.ECDH(uaKey)
	if err != nil {
		return nil, fmt.Errorf("key agreement: %w", err)
	}
	asPublic := asKey.PublicKey().Bytes()
	cek, nonce, err := deriveKeys(shared, authSecret, uaKey.Bytes(), asPublic, salt)
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	// 0x02 marks the last record; everything sent here fits in one.
	padded := append(append([]byte{}, plaintext...), 0x02)

	header := make([]byte, 0, len(salt)+4+1+len(asPublic))
	header = append(header, salt...)
	header = binary.BigEndian.AppendUint32(header, recordSize)
	header = append(header, byte(len(asPublic)))
	header = append(header, asPublic...)

	return gcm.Seal(header, nonce, padded, nil), nil
}

// deriveKeys follows RFC 8291 section 3.4. It takes the agreed secret rather
// than a key pair because both sides run the identical derivation.
func deriveKeys(shared, authSecret, uaPublic, asPublic, salt []byte) (cek, nonce []byte, err error) {
	prkKey, err := hkdf.Extract(sha256.New, shared, authSecret)
	if err != nil {
		return nil, nil, err
	}
	keyInfo := append([]byte("WebPush: info\x00"), uaPublic...)
	keyInfo = append(keyInfo, asPublic...)
	ikm, err := hkdf.Expand(sha256.New, prkKey, string(keyInfo), sha256.Size)
	if err != nil {
		return nil, nil, err
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, nil, err
	}
	if cek, err = hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16); err != nil {
		return nil, nil, err
	}
	if nonce, err = hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12); err != nil {
		return nil, nil, err
	}
	return cek, nonce, nil
}

// encryptRandom is the production path. Reusing a key pair or salt across
// messages would leak the plaintext, so each gets its own.
func encryptRandom(uaPublic, authSecret, plaintext []byte) ([]byte, error) {
	asKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	return encrypt(uaPublic, authSecret, plaintext, asKey, salt)
}
