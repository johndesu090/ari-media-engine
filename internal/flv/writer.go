// Package flv frames media tags into the FLV container format used by RTMP.
// It is shared by the transcoder (piping to FFmpeg) and the recorder (writing
// to disk or object storage).
package flv

import (
	"encoding/binary"
	"io"
)

// Writer frames media tags into an FLV byte stream. The 9-byte FLV header and
// PreviousTagSize0 are written lazily on the first tag so that a header is only
// emitted once media is actually available.
type Writer struct {
	w       io.Writer
	started bool
}

// NewWriter wraps w in an FLV Writer.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// WriteTag writes an FLV tag: an 11-byte tag header, the body and a 4-byte
// previous-tag size. tagType is 8 (audio), 9 (video) or 18 (script data).
func (f *Writer) WriteTag(tagType uint8, timestamp uint32, data []byte) error {
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

// Started reports whether the FLV header has been written.
func (f *Writer) Started() bool { return f.started }
