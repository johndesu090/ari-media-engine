package rtmp

import (
	"fmt"
	"io"
	"sync"
)

// ChunkWriter serialises RTMP messages onto a connection, splitting payloads
// across chunks when they exceed the outbound chunk size.
type ChunkWriter struct {
	mu        sync.Mutex
	w         io.Writer
	chunkSize uint32
}

// NewChunkWriter wraps w with the default chunk size.
func NewChunkWriter(w io.Writer) *ChunkWriter {
	return &ChunkWriter{w: w, chunkSize: DefaultChunkSize}
}

// SetChunkSize updates the outbound chunk size.
func (c *ChunkWriter) SetChunkSize(n uint32) {
	if n > 0 && n <= maxChunkSize {
		c.chunkSize = n
	}
}

// WriteMessage writes one message using a format-0 header and format-3
// continuations. Only the single-byte chunk stream ids (2..63) are supported,
// which covers all server-to-client control and command messages.
func (c *ChunkWriter) WriteMessage(csid uint32, msgType uint8, streamID, timestamp uint32, payload []byte) error {
	if csid < 2 || csid > 63 {
		return fmt.Errorf("rtmp: unsupported outbound csid %d", csid)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	extended := timestamp >= 0xFFFFFF
	hdr := make([]byte, 0, 16)
	hdr = append(hdr, byte(csid))
	if extended {
		hdr = append(hdr, 0xFF, 0xFF, 0xFF)
	} else {
		hdr = append(hdr, byte(timestamp>>16), byte(timestamp>>8), byte(timestamp))
	}
	n := len(payload)
	hdr = append(hdr, byte(n>>16), byte(n>>8), byte(n))
	hdr = append(hdr, msgType)
	hdr = append(hdr, byte(streamID), byte(streamID>>8), byte(streamID>>16), byte(streamID>>24))
	if extended {
		hdr = append(hdr, byte(timestamp>>24), byte(timestamp>>16), byte(timestamp>>8), byte(timestamp))
	}
	if _, err := c.w.Write(hdr); err != nil {
		return err
	}

	for offset := 0; ; {
		end := offset + int(c.chunkSize)
		if end > len(payload) {
			end = len(payload)
		}
		if _, err := c.w.Write(payload[offset:end]); err != nil {
			return err
		}
		offset = end
		if offset >= len(payload) {
			break
		}
		cont := []byte{0xC0 | byte(csid)}
		if extended {
			cont = append(cont, byte(timestamp>>24), byte(timestamp>>16), byte(timestamp>>8), byte(timestamp))
		}
		if _, err := c.w.Write(cont); err != nil {
			return err
		}
	}
	return nil
}
