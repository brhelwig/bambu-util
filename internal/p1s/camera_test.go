package p1s

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"io"
	"math/big"
	"net"
	"testing"
	"time"
)

func TestAuthPacketLayout(t *testing.T) {
	p, err := AuthPacket("bblp", "12345678")
	if err != nil {
		t.Fatal(err)
	}
	if len(p) != 80 {
		t.Fatalf("len = %d, want 80", len(p))
	}
	if binary.LittleEndian.Uint32(p[0:]) != 0x40 || binary.LittleEndian.Uint32(p[4:]) != 0x3000 {
		t.Fatal("bad magic words")
	}
	if !bytes.Equal(p[16:20], []byte("bblp")) || p[20] != 0 {
		t.Fatal("username not at offset 16, zero-padded")
	}
	if !bytes.Equal(p[48:56], []byte("12345678")) || p[56] != 0 {
		t.Fatal("access code not at offset 48, zero-padded")
	}
}

func TestAuthPacketRejectsOverlongInputs(t *testing.T) {
	if _, err := AuthPacket("bblp", "123456789012345678901234567890123"); err == nil {
		t.Fatal("accepted 33-byte access code")
	}
}

func frameBytes(jpeg []byte) []byte {
	var b bytes.Buffer
	var header [16]byte
	binary.LittleEndian.PutUint32(header[0:], uint32(len(jpeg)))
	b.Write(header[:])
	b.Write(jpeg)
	return b.Bytes()
}

func TestReadFrameRoundTrip(t *testing.T) {
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x01, 0x02, 0xFF, 0xD9}
	got, err := ReadFrame(bytes.NewReader(frameBytes(jpeg)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, jpeg) {
		t.Fatalf("frame mismatch: %x", got)
	}
}

func TestReadFrameRejectsBadMagic(t *testing.T) {
	if _, err := ReadFrame(bytes.NewReader(frameBytes([]byte{1, 2, 3, 4, 5, 6}))); err == nil {
		t.Fatal("accepted non-JPEG payload")
	}
}

func TestReadFrameRejectsImplausibleSize(t *testing.T) {
	var header [16]byte
	binary.LittleEndian.PutUint32(header[0:], 100<<20)
	if _, err := ReadFrame(bytes.NewReader(header[:])); err == nil {
		t.Fatal("accepted 100MB frame size")
	}
}

func TestReadFrameShortRead(t *testing.T) {
	full := frameBytes([]byte{0xFF, 0xD8, 0xFF, 0xD9})
	if _, err := ReadFrame(bytes.NewReader(full[:len(full)-2])); err == nil {
		t.Fatal("accepted truncated frame")
	}
	if _, err := ReadFrame(bytes.NewReader(nil)); err != io.EOF {
		t.Fatalf("empty reader: want io.EOF, got %v", err)
	}
}

func newTLSListener(t *testing.T) net.Listener {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln
}

func TestStreamFrames(t *testing.T) {
	ln := newTLSListener(t)
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xD9}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		auth := make([]byte, 80)
		if _, err := io.ReadFull(conn, auth); err != nil {
			t.Errorf("auth read: %v", err)
			return
		}
		if !bytes.Equal(auth[16:20], []byte("bblp")) {
			t.Errorf("bad auth packet: %x", auth)
		}
		conn.Write(frameBytes(jpeg))
		conn.Write(frameBytes(jpeg))
	}()

	var got [][]byte
	err := StreamFrames(context.Background(), ln.Addr().String(), "bblp", "12345678",
		func(f []byte) { got = append(got, f) })
	if err == nil {
		t.Fatal("expected error when server closes")
	}
	if len(got) != 2 {
		t.Fatalf("got %d frames, want 2", len(got))
	}
}

func TestStreamFramesCancellation(t *testing.T) {
	ln := newTLSListener(t)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		io.ReadFull(conn, make([]byte, 80))
		// send nothing more; client should block until cancelled
		time.Sleep(5 * time.Second)
	}()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	err := StreamFrames(ctx, ln.Addr().String(), "bblp", "12345678", func([]byte) {})
	if err != context.Canceled {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}
