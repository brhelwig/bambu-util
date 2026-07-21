package p1s

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
)

// AuthPacket builds the 80-byte camera handshake: LE 0x40, LE 0x3000, eight
// zero bytes, then username and access code each zero-padded to 32 bytes.
// Layout matches ha-bambulab's ChamberImageThread.
func AuthPacket(username, accessCode string) ([]byte, error) {
	if len(username) > 32 || len(accessCode) > 32 {
		return nil, fmt.Errorf("username/access code longer than 32 bytes")
	}
	p := make([]byte, 80)
	binary.LittleEndian.PutUint32(p[0:], 0x40)
	binary.LittleEndian.PutUint32(p[4:], 0x3000)
	copy(p[16:], username)
	copy(p[48:], accessCode)
	return p, nil
}

const maxFrameSize = 8 << 20 // chamber JPEGs are tens of KB; 8MB means corruption

// ReadFrame reads one camera frame: a 16-byte header whose first four bytes
// are the little-endian JPEG size, then the JPEG itself.
func ReadFrame(r io.Reader) ([]byte, error) {
	var header [16]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	size := binary.LittleEndian.Uint32(header[0:4])
	if size < 4 || size > maxFrameSize {
		return nil, fmt.Errorf("implausible frame size %d", size)
	}
	img := make([]byte, size)
	if _, err := io.ReadFull(r, img); err != nil {
		return nil, fmt.Errorf("truncated frame: %w", err)
	}
	if img[0] != 0xFF || img[1] != 0xD8 {
		return nil, fmt.Errorf("frame missing JPEG start marker")
	}
	if img[size-2] != 0xFF || img[size-1] != 0xD9 {
		return nil, fmt.Errorf("frame missing JPEG end marker")
	}
	return img, nil
}

// StreamFrames dials the camera port, authenticates, and passes each JPEG to
// yield until ctx is cancelled or the connection breaks.
func StreamFrames(ctx context.Context, addr, username, accessCode string, yield func([]byte)) error {
	auth, err := AuthPacket(username, accessCode)
	if err != nil {
		return err
	}
	d := tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true}} // printer cert is self-signed
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() }) // unblock blocked reads
	defer stop()
	if _, err := conn.Write(auth); err != nil {
		return err
	}
	for {
		frame, err := ReadFrame(conn)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		yield(frame)
	}
}
