package transcode

import (
	"encoding/binary"
	"io"
)

// flvWriter frames RTMP media messages into an FLV byte stream for FFmpeg. The
// FLV container is written lazily on the first tag so that an FLV header is only
// emitted once media is actually available.
type flvWriter struct {
	w       io.Writer
	started bool
}

func newFLVWriter(w io.Writer) *flvWriter { return &flvWriter{w: w} }

// writeTag writes an FLV tag (11-byte header + body + 4-byte previous-tag size),
// emitting the 9-byte FLV header and PreviousTagSize0 on the first call.
func (f *flvWriter) writeTag(tagType uint8, timestamp uint32, data []byte) error {
	if !f.started {
		// "FLV", version 1, flags (audio|video = 0x05), header size 9.
		if _, err := f.w.Write([]byte{'F', 'L', 'V', 0x01, 0x05, 0x00, 0x00, 0x00, 0x09}); err != nil {
			return err
		}
		if _, err := f.w.Write([]byte{0, 0, 0, 0}); err != nil {
			return err
		}
		f.started = true
	}

	n := len(data)
	var tag [11]byte
	tag[0] = tagType
	tag[1] = byte(n >> 16)
	tag[2] = byte(n >> 8)
	tag[3] = byte(n)
	tag[4] = byte(timestamp >> 16)
	tag[5] = byte(timestamp >> 8)
	tag[6] = byte(timestamp)
	tag[7] = byte(timestamp >> 24)
	// bytes 8..10 are the (always zero) stream id.

	if _, err := f.w.Write(tag[:]); err != nil {
		return err
	}
	if _, err := f.w.Write(data); err != nil {
		return err
	}

	var prev [4]byte
	binary.BigEndian.PutUint32(prev[:], uint32(11+n))
	_, err := f.w.Write(prev[:])
	return err
}
