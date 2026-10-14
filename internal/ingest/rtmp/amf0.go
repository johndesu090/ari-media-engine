// Package rtmp implements the subset of RTMP required for live publishing:
// the simple handshake, chunk-stream reassembly, AMF0 command encoding and
// decoding, and FLV audio/video tag demultiplexing.
package rtmp

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// AMF0 type markers.
const (
	amf0Number      = 0x00
	amf0Boolean     = 0x01
	amf0String      = 0x02
	amf0Object      = 0x03
	amf0Null        = 0x05
	amf0Undefined   = 0x06
	amf0ECMAArray   = 0x08
	amf0ObjectEnd   = 0x09
	amf0StrictArray = 0x0A
	amf0LongString  = 0x0C
)

// ErrUnsupportedAMF is returned for AMF markers the engine does not decode.
var ErrUnsupportedAMF = errors.New("rtmp: unsupported AMF0 type")

// Decoder decodes AMF0 values from a stream.
type Decoder struct {
	r *bufio.Reader
}

// NewDecoder wraps r in an AMF0 Decoder.
func NewDecoder(r io.Reader) *Decoder {
	if br, ok := r.(*bufio.Reader); ok {
		return &Decoder{r: br}
	}
	return &Decoder{r: bufio.NewReader(r)}
}

// Decode reads and returns the next AMF0 value. Objects and ECMA arrays become
// map[string]any, strict arrays become []any, and null/undefined become nil.
func (d *Decoder) Decode() (any, error) {
	marker, err := d.r.ReadByte()
	if err != nil {
		return nil, err
	}
	return d.decodeValue(marker)
}

func (d *Decoder) decodeValue(marker byte) (any, error) {
	switch marker {
	case amf0Number:
		var f float64
		if err := binary.Read(d.r, binary.BigEndian, &f); err != nil {
			return nil, err
		}
		return f, nil
	case amf0Boolean:
		b, err := d.r.ReadByte()
		if err != nil {
			return nil, err
		}
		return b != 0, nil
	case amf0String:
		return d.readString()
	case amf0LongString:
		var n uint32
		if err := binary.Read(d.r, binary.BigEndian, &n); err != nil {
			return nil, err
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(d.r, buf); err != nil {
			return nil, err
		}
		return string(buf), nil
	case amf0Object:
		return d.readObject()
	case amf0ECMAArray:
		var n uint32
		if err := binary.Read(d.r, binary.BigEndian, &n); err != nil {
			return nil, err
		}
		return d.readObject()
	case amf0Null, amf0Undefined:
		return nil, nil
	case amf0StrictArray:
		var n uint32
		if err := binary.Read(d.r, binary.BigEndian, &n); err != nil {
			return nil, err
		}
		arr := make([]any, 0, n)
		for i := uint32(0); i < n; i++ {
			m, err := d.r.ReadByte()
			if err != nil {
				return nil, err
			}
			v, err := d.decodeValue(m)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		return arr, nil
	default:
		return nil, fmt.Errorf("%w: 0x%02x", ErrUnsupportedAMF, marker)
	}
}

func (d *Decoder) readString() (string, error) {
	var n uint16
	if err := binary.Read(d.r, binary.BigEndian, &n); err != nil {
		return "", err
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(d.r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

func (d *Decoder) readObject() (map[string]any, error) {
	obj := map[string]any{}
	for {
		var n uint16
		if err := binary.Read(d.r, binary.BigEndian, &n); err != nil {
			return nil, err
		}
		if n == 0 {
			marker, err := d.r.ReadByte()
			if err != nil {
				return nil, err
			}
			if marker == amf0ObjectEnd {
				return obj, nil
			}
			return nil, fmt.Errorf("rtmp: malformed object terminator 0x%02x", marker)
		}
		key := make([]byte, n)
		if _, err := io.ReadFull(d.r, key); err != nil {
			return nil, err
		}
		marker, err := d.r.ReadByte()
		if err != nil {
			return nil, err
		}
		val, err := d.decodeValue(marker)
		if err != nil {
			return nil, err
		}
		obj[string(key)] = val
	}
}

// Encoder writes AMF0 values to a stream.
type Encoder struct {
	w io.Writer
}

// NewEncoder wraps w in an AMF0 Encoder.
func NewEncoder(w io.Writer) *Encoder { return &Encoder{w: w} }

func (e *Encoder) marker(m byte) error {
	_, err := e.w.Write([]byte{m})
	return err
}

// WriteString writes a string, using the long-string form when required.
func (e *Encoder) WriteString(s string) error {
	if len(s) > 0xFFFF {
		if err := e.marker(amf0LongString); err != nil {
			return err
		}
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], uint32(len(s)))
		if _, err := e.w.Write(b[:]); err != nil {
			return err
		}
		_, err := io.WriteString(e.w, s)
		return err
	}
	if err := e.marker(amf0String); err != nil {
		return err
	}
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], uint16(len(s)))
	if _, err := e.w.Write(b[:]); err != nil {
		return err
	}
	_, err := io.WriteString(e.w, s)
	return err
}

// WriteNumber writes a double.
func (e *Encoder) WriteNumber(f float64) error {
	if err := e.marker(amf0Number); err != nil {
		return err
	}
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], math.Float64bits(f))
	_, err := e.w.Write(b[:])
	return err
}

// WriteBool writes a boolean.
func (e *Encoder) WriteBool(v bool) error {
	if err := e.marker(amf0Boolean); err != nil {
		return err
	}
	var b byte
	if v {
		b = 1
	}
	_, err := e.w.Write([]byte{b})
	return err
}

// WriteNull writes a null value.
func (e *Encoder) WriteNull() error { return e.marker(amf0Null) }

// WriteObject writes an object with the given key/value pairs.
func (e *Encoder) WriteObject(m map[string]any) error {
	if err := e.marker(amf0Object); err != nil {
		return err
	}
	return e.writeObjectBody(m)
}

// WriteECMAArray writes an ECMA (associative) array.
func (e *Encoder) WriteECMAArray(m map[string]any) error {
	if err := e.marker(amf0ECMAArray); err != nil {
		return err
	}
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(len(m)))
	if _, err := e.w.Write(b[:]); err != nil {
		return err
	}
	return e.writeObjectBody(m)
}

func (e *Encoder) writeObjectBody(m map[string]any) error {
	for k, v := range m {
		if err := e.writeKey(k); err != nil {
			return err
		}
		if err := e.WriteValue(v); err != nil {
			return err
		}
	}
	// object end: empty key + object-end marker
	if _, err := e.w.Write([]byte{0x00, 0x00, amf0ObjectEnd}); err != nil {
		return err
	}
	return nil
}

func (e *Encoder) writeKey(k string) error {
	if len(k) > 0xFFFF {
		k = k[:0xFFFF]
	}
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], uint16(len(k)))
	if _, err := e.w.Write(b[:]); err != nil {
		return err
	}
	_, err := io.WriteString(e.w, k)
	return err
}

// WriteValue writes an arbitrary Go value using the closest AMF0 type.
func (e *Encoder) WriteValue(v any) error {
	switch t := v.(type) {
	case nil:
		return e.WriteNull()
	case string:
		return e.WriteString(t)
	case bool:
		return e.WriteBool(t)
	case float64:
		return e.WriteNumber(t)
	case int:
		return e.WriteNumber(float64(t))
	case map[string]any:
		return e.WriteObject(t)
	default:
		return e.WriteString(fmt.Sprint(t))
	}
}
