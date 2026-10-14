package rtmp

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"time"
)

// MediaSink consumes demuxed media for one published stream.
type MediaSink interface {
	// SetVideoConfig receives the H.264 SPS/PPS from the AVC sequence header.
	SetVideoConfig(sps, pps []byte) error
	// WriteVideo receives an H.264 access unit as NAL units with the RTMP
	// timestamp in milliseconds.
	WriteVideo(nalus [][]byte, ptsMillis uint32, keyframe bool) error
	// Close releases the sink.
	Close()
}

// AudioSink is optionally implemented by a MediaSink that can consume AAC.
type AudioSink interface {
	WriteAudio(frame []byte, ptsMillis uint32, config bool) error
}

// FLVSink is optionally implemented by a MediaSink that wants the raw FLV tag
// bodies (for recording or FFmpeg-based transcoding) in addition to demuxed
// media.
type FLVSink interface {
	WriteFLVTag(tagType uint8, timestamp uint32, data []byte) error
	Close()
}

// MultiFLVSink forwards each tag to every non-nil sink. It is a no-op when no
// sinks are supplied.
func MultiFLVSink(sinks ...FLVSink) FLVSink {
	var live []FLVSink
	for _, s := range sinks {
		if s != nil {
			live = append(live, s)
		}
	}
	return &multiFLVSink{sinks: live}
}

type multiFLVSink struct{ sinks []FLVSink }

func (m *multiFLVSink) WriteFLVTag(tagType uint8, timestamp uint32, data []byte) error {
	for _, s := range m.sinks {
		if err := s.WriteFLVTag(tagType, timestamp, data); err != nil {
			return err
		}
	}
	return nil
}

func (m *multiFLVSink) Close() {
	for _, s := range m.sinks {
		s.Close()
	}
}

// SinkFactory opens a media sink when a publisher starts.
type SinkFactory interface {
	Open(streamKey string) (MediaSink, error)
}

// Server handles inbound RTMP publisher connections.
type Server struct {
	Sinks       SinkFactory
	Logger      *slog.Logger
	OnPublish   func(streamKey string)
	OnUnpublish func(streamKey string)

	// OutChunkSize is advertised to the client (default 4096).
	OutChunkSize uint32
	// ReadTimeout bounds idle time between messages (default 60s).
	ReadTimeout time.Duration
}

// Serve performs the RTMP handshake and runs the publisher command loop until
// the connection closes or ctx is cancelled.
func (s *Server) Serve(ctx context.Context, conn net.Conn) error {
	log := s.Logger
	if log == nil {
		log = slog.Default()
	}
	if err := ServerHandshake(conn); err != nil {
		return err
	}

	sess := &session{
		server: s,
		conn:   conn,
		reader: NewChunkReader(conn),
		writer: NewChunkWriter(conn),
		log:    log,
	}
	defer sess.cleanup()

	timeout := s.ReadTimeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}

	for {
		if ctx != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
		}
		_ = conn.SetReadDeadline(time.Now().Add(timeout))
		msg, err := sess.reader.ReadMessage()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		if err := sess.handle(msg); err != nil {
			return err
		}
	}
}

type session struct {
	server *Server
	conn   net.Conn
	reader *ChunkReader
	writer *ChunkWriter
	log    *slog.Logger

	app        string
	streamKey  string
	sink       MediaSink
	publishing bool
	streamID   uint32
	lengthSize int
}

func (s *session) cleanup() {
	if s.sink != nil {
		s.sink.Close()
		if s.server.OnUnpublish != nil && s.streamKey != "" {
			s.server.OnUnpublish(s.streamKey)
		}
		s.log.Info("rtmp publisher stopped", "stream", s.streamKey)
	}
}

func (s *session) handle(msg Message) error {
	switch msg.Type {
	case MsgSetChunkSize:
		if len(msg.Payload) >= 4 {
			s.reader.SetChunkSize(binary.BigEndian.Uint32(msg.Payload[:4]) & 0x7fffffff)
		}
	case MsgCommandAMF0:
		return s.handleCommand(msg)
	case MsgVideo:
		if err := s.handleVideo(msg); err != nil {
			return err
		}
		return s.forwardFLV(msg)
	case MsgAudio:
		if err := s.handleAudio(msg); err != nil {
			return err
		}
		return s.forwardFLV(msg)
	case MsgDataAMF0:
		return s.forwardFLV(msg)
	}
	return nil
}

// forwardFLV forwards a raw FLV tag body to the sink when it implements FLVSink.
func (s *session) forwardFLV(msg Message) error {
	if !s.publishing || s.sink == nil {
		return nil
	}
	if fs, ok := s.sink.(FLVSink); ok {
		return fs.WriteFLVTag(msg.Type, msg.Timestamp, msg.Payload)
	}
	return nil
}

func (s *session) handleCommand(msg Message) error {
	dec := NewDecoder(bytes.NewReader(msg.Payload))
	var vals []any
	for {
		v, err := dec.Decode()
		if err != nil {
			break
		}
		vals = append(vals, v)
	}
	if len(vals) == 0 {
		return nil
	}
	name, _ := vals[0].(string)
	txn := 0.0
	if len(vals) > 1 {
		if f, ok := vals[1].(float64); ok {
			txn = f
		}
	}
	s.log.Debug("rtmp command", "name", name, "txn", txn)

	switch name {
	case "connect":
		return s.handleConnect(txn, vals)
	case "createStream":
		return s.sendCommand(3, 0, "_result", txn, nil, 1.0)
	case "publish":
		return s.handlePublish(vals)
	case "play":
		return s.sendStatus("error", "NetStream.Play.Failed", "playback is not supported on the publish endpoint")
	case "releaseStream", "FCPublish", "FCUnpublish", "deleteStream", "closeStream", "_checkbw":
		if txn != 0 {
			return s.sendCommand(3, 0, "_result", txn, nil, nil)
		}
		return nil
	default:
		if txn != 0 {
			return s.sendCommand(3, 0, "_result", txn, nil, nil)
		}
		return nil
	}
}

func (s *session) handleConnect(txn float64, vals []any) error {
	if len(vals) > 2 {
		if obj, ok := vals[2].(map[string]any); ok {
			if app, ok := obj["app"].(string); ok {
				s.app = app
			}
		}
	}

	if err := s.sendProtocol(MsgWindowAckSize, uint32be(2500000)); err != nil {
		return err
	}
	// Set Peer Bandwidth: 4-byte window + 1-byte limit type (2 = dynamic).
	if err := s.sendProtocol(MsgSetPeerBandwidth, append(uint32be(2500000), 2)); err != nil {
		return err
	}
	size := s.server.OutChunkSize
	if size == 0 {
		size = 4096
	}
	if err := s.sendProtocol(MsgSetChunkSize, uint32be(size)); err != nil {
		return err
	}
	s.writer.SetChunkSize(size)

	props := map[string]any{
		"fmsVer":       "FMS/3,5,7,7009",
		"capabilities": 31.0,
		"mode":         1.0,
	}
	info := map[string]any{
		"level":          "status",
		"code":           "NetConnection.Connect.Success",
		"description":    "Connection succeeded.",
		"objectEncoding": 0.0,
	}
	return s.sendCommand(3, 0, "_result", txn, props, info)
}

func (s *session) handlePublish(vals []any) error {
	streamKey := ""
	// Expected layout: ["publish", txn, null, streamKey, publishType].
	if len(vals) > 3 {
		if sk, ok := vals[3].(string); ok {
			streamKey = sk
		}
	}
	if streamKey == "" {
		for _, v := range vals {
			if sk, ok := v.(string); ok && sk != "publish" && sk != "live" && sk != "record" && sk != "append" {
				streamKey = sk
			}
		}
	}
	if streamKey == "" {
		return s.sendStatus("error", "NetStream.Publish.BadName", "missing stream key")
	}
	if s.server.Sinks == nil {
		return s.sendStatus("error", "NetStream.Publish.Failed", "ingest is not enabled")
	}

	sink, err := s.server.Sinks.Open(streamKey)
	if err != nil {
		s.log.Warn("rtmp publish rejected", "stream", streamKey, "err", err)
		return s.sendStatus("error", "NetStream.Publish.Failed", err.Error())
	}

	s.sink = sink
	s.streamKey = streamKey
	s.publishing = true
	s.streamID = 1
	s.log.Info("rtmp publisher started", "stream", streamKey, "app", s.app)
	if s.server.OnPublish != nil {
		s.server.OnPublish(streamKey)
	}
	return s.sendStatus("status", "NetStream.Publish.Start", "Publishing "+streamKey)
}

func (s *session) handleVideo(msg Message) error {
	if !s.publishing || s.sink == nil || len(msg.Payload) < 1 {
		return nil
	}
	p := msg.Payload
	frameType := p[0] >> 4
	if p[0]&0x0f != flvCodecAVC || len(p) < 5 {
		return nil
	}
	switch p[1] {
	case avcSeqHeader:
		cfg, err := parseAVCConfig(p[5:])
		if err != nil {
			return nil
		}
		s.lengthSize = cfg.lengthSize
		return s.sink.SetVideoConfig(cfg.sps, cfg.pps)
	case avcNALU:
		nalus, err := parseAVCCSample(p[5:], s.lengthSize)
		if err != nil || len(nalus) == 0 {
			return nil
		}
		return s.sink.WriteVideo(nalus, msg.Timestamp, frameType == flvFrameKey)
	}
	return nil
}

func (s *session) handleAudio(msg Message) error {
	if !s.publishing || s.sink == nil || len(msg.Payload) < 2 {
		return nil
	}
	if msg.Payload[0]>>4 != flvSoundAAC {
		return nil
	}
	// AudioSpecificConfig (0) or raw AAC (1). The built-in engine has no audio
	// track yet; a transcoder implementing AudioSink will consume this.
	if as, ok := s.sink.(AudioSink); ok {
		return as.WriteAudio(msg.Payload[2:], msg.Timestamp, msg.Payload[1] == aacSeqHeader)
	}
	return nil
}

func (s *session) sendProtocol(msgType uint8, payload []byte) error {
	return s.writer.WriteMessage(2, msgType, 0, 0, payload)
}

func (s *session) sendCommand(csid uint32, streamID uint32, name string, txn float64, args ...any) error {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	if err := enc.WriteString(name); err != nil {
		return err
	}
	if err := enc.WriteNumber(txn); err != nil {
		return err
	}
	for _, a := range args {
		if err := enc.WriteValue(a); err != nil {
			return err
		}
	}
	return s.writer.WriteMessage(csid, MsgCommandAMF0, streamID, 0, buf.Bytes())
}

func (s *session) sendStatus(level, code, description string) error {
	streamID := s.streamID
	if streamID == 0 {
		streamID = 1
	}
	info := map[string]any{"level": level, "code": code, "description": description}
	return s.sendCommand(5, streamID, "onStatus", 0, nil, info)
}

func uint32be(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}
