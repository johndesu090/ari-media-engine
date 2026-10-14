package rtmp

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
)

// RTMP message type IDs used by the engine.
const (
	MsgSetChunkSize     = 1
	MsgAbort            = 2
	MsgAck              = 3
	MsgUserControl      = 4
	MsgWindowAckSize    = 5
	MsgSetPeerBandwidth = 6
	MsgAudio            = 8
	MsgVideo            = 9
	MsgDataAMF0         = 18
	MsgCommandAMF0      = 20
)

// Default and maximum chunk sizes.
const (
	DefaultChunkSize = 128
	maxChunkSize     = 0xFFFFFF
)

// Message is a fully reassembled RTMP message.
type Message struct {
	CSID      uint32
	Type      uint8
	StreamID  uint32
	Timestamp uint32
	Payload   []byte
}

type chunkState struct {
	timestamp uint32
	msgLen    uint32
	msgType   uint8
	streamID  uint32
	hasHeader bool
	extended  bool
	buf       []byte
}

// ChunkReader reassembles RTMP messages from a connection.
type ChunkReader struct {
	r         *bufio.Reader
	chunkSize uint32
	states    map[uint32]*chunkState
}

// NewChunkReader wraps r in a ChunkReader with the default chunk size.
func NewChunkReader(r io.Reader) *ChunkReader {
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReader(r)
	}
	return &ChunkReader{r: br, chunkSize: DefaultChunkSize, states: map[uint32]*chunkState{}}
}

// SetChunkSize updates the inbound chunk size (from a Set Chunk Size message).
func (c *ChunkReader) SetChunkSize(n uint32) {
	if n > 0 && n <= maxChunkSize {
		c.chunkSize = n
	}
}

// ReadMessage reassembles and returns the next complete RTMP message.
func (c *ChunkReader) ReadMessage() (Message, error) {
	for {
		b0, err := c.r.ReadByte()
		if err != nil {
			return Message{}, err
		}
		format := b0 >> 6
		csid := uint32(b0 & 0x3f)
		switch csid {
		case 0:
			b, err := c.r.ReadByte()
			if err != nil {
				return Message{}, err
			}
			csid = 64 + uint32(b)
		case 1:
			var bb [2]byte
			if _, err := io.ReadFull(c.r, bb[:]); err != nil {
				return Message{}, err
			}
			csid = 64 + uint32(bb[0]) + uint32(bb[1])*256
		}

		st := c.states[csid]
		if st == nil {
			st = &chunkState{}
			c.states[csid] = st
		}
		if err := c.readHeader(st, format); err != nil {
			return Message{}, err
		}
		if !st.hasHeader {
			return Message{}, fmt.Errorf("rtmp: chunk on csid %d without a prior header", csid)
		}

		remaining := int(st.msgLen) - len(st.buf)
		if remaining <= 0 {
			return c.emit(csid, st), nil
		}
		n := int(c.chunkSize)
		if remaining < n {
			n = remaining
		}
		tmp := make([]byte, n)
		if _, err := io.ReadFull(c.r, tmp); err != nil {
			return Message{}, err
		}
		st.buf = append(st.buf, tmp...)
		if len(st.buf) >= int(st.msgLen) {
			return c.emit(csid, st), nil
		}
	}
}

func (c *ChunkReader) emit(csid uint32, st *chunkState) Message {
	msg := Message{CSID: csid, Type: st.msgType, StreamID: st.streamID, Timestamp: st.timestamp, Payload: st.buf}
	st.buf = nil
	return msg
}

func (c *ChunkReader) readHeader(st *chunkState, format byte) error {
	switch format {
	case 0:
		var b [11]byte
		if _, err := io.ReadFull(c.r, b[:]); err != nil {
			return err
		}
		ts := uint24(b[0:3])
		st.msgLen = uint24(b[3:6])
		st.msgType = b[6]
		st.streamID = binary.LittleEndian.Uint32(b[7:11])
		st.timestamp = ts
		st.hasHeader = true
		st.extended = ts == 0xFFFFFF
		if st.extended {
			ext, err := c.readExtended()
			if err != nil {
				return err
			}
			st.timestamp = ext
		}
	case 1:
		var b [7]byte
		if _, err := io.ReadFull(c.r, b[:]); err != nil {
			return err
		}
		delta := uint24(b[0:3])
		st.msgLen = uint24(b[3:6])
		st.msgType = b[6]
		st.timestamp += delta
		st.extended = delta == 0xFFFFFF
		if st.extended {
			ext, err := c.readExtended()
			if err != nil {
				return err
			}
			st.timestamp += ext
		}
	case 2:
		var b [3]byte
		if _, err := io.ReadFull(c.r, b[:]); err != nil {
			return err
		}
		delta := uint24(b[:])
		st.timestamp += delta
		st.extended = delta == 0xFFFFFF
		if st.extended {
			ext, err := c.readExtended()
			if err != nil {
				return err
			}
			st.timestamp += ext
		}
	case 3:
		if st.extended {
			if _, err := c.readExtended(); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("rtmp: invalid chunk format %d", format)
	}
	return nil
}

func (c *ChunkReader) readExtended() (uint32, error) {
	var ext [4]byte
	if _, err := io.ReadFull(c.r, ext[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(ext[:]), nil
}

func uint24(b []byte) uint32 {
	return uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2])
}
