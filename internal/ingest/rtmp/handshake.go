package rtmp

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"time"
)

// Handshake sizes for the RTMP simple handshake.
const (
	handshakeVersion = 0x03
	handshakeSize    = 1536
)

// ServerHandshake performs the RTMP server side of the simple handshake over
// rw. On success the connection is ready to carry chunk streams.
func ServerHandshake(rw io.ReadWriter) error {
	// C0 + C1
	var c0c1 [1 + handshakeSize]byte
	if _, err := io.ReadFull(rw, c0c1[:]); err != nil {
		return fmt.Errorf("rtmp: read C0/C1: %w", err)
	}
	if c0c1[0] != handshakeVersion {
		return fmt.Errorf("rtmp: unsupported handshake version %d", c0c1[0])
	}

	// S0
	if _, err := rw.Write([]byte{handshakeVersion}); err != nil {
		return fmt.Errorf("rtmp: write S0: %w", err)
	}

	// S1: time(4) + zero(4) + random(1528)
	var s1 [handshakeSize]byte
	binary.BigEndian.PutUint32(s1[0:4], uint32(time.Now().UnixMilli()))
	if _, err := rand.Read(s1[8:]); err != nil {
		return fmt.Errorf("rtmp: random S1: %w", err)
	}
	if _, err := rw.Write(s1[:]); err != nil {
		return fmt.Errorf("rtmp: write S1: %w", err)
	}

	// S2: echo of C1
	if _, err := rw.Write(c0c1[1:]); err != nil {
		return fmt.Errorf("rtmp: write S2: %w", err)
	}

	// C2
	var c2 [handshakeSize]byte
	if _, err := io.ReadFull(rw, c2[:]); err != nil {
		return fmt.Errorf("rtmp: read C2: %w", err)
	}
	return nil
}
