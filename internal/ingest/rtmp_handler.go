package ingest

import (
	"context"
	"log/slog"
	"net"

	"github.com/johndesu090/ari-media-engine/internal/ingest/rtmp"
	"github.com/johndesu090/ari-media-engine/internal/webrtc"
)

// IngestEngine is the subset of the WebRTC engine needed to publish demuxed
// media into a stream.
type IngestEngine interface {
	Ingest(streamID, sessionID string) (*webrtc.IngestSession, error)
}

// RTMPHooks receive RTMP publish lifecycle notifications.
type RTMPHooks struct {
	OnPublish   func(streamKey string)
	OnUnpublish func(streamKey string)
}

// RTMPOptions configures an RTMPHandler.
type RTMPOptions struct {
	Engine IngestEngine
	Logger *slog.Logger
	Hooks  RTMPHooks
	// NewFLVSink, when set, opens an additional sink that receives the raw FLV
	// tag stream (used for transcoding and recording).
	NewFLVSink func(streamKey string) (rtmp.FLVSink, error)
	// NewAudioSink, when set, opens an audio sink bound to the stream's ingest
	// session (used to transcode AAC audio to Opus for WebRTC).
	NewAudioSink func(streamKey string, session *webrtc.IngestSession) (rtmp.FLVSink, error)
}

// RTMPHandler adapts RTMP publisher connections onto the WebRTC ingest pipeline.
type RTMPHandler struct {
	server *rtmp.Server
}

// NewRTMPHandler builds a Handler that serves RTMP publishers into opts.Engine.
func NewRTMPHandler(opts RTMPOptions) *RTMPHandler {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &RTMPHandler{
		server: &rtmp.Server{
			Sinks:       &engineSinkFactory{engine: opts.Engine, newFLV: opts.NewFLVSink, newAudio: opts.NewAudioSink, log: log},
			Logger:      log,
			OnPublish:   opts.Hooks.OnPublish,
			OnUnpublish: opts.Hooks.OnUnpublish,
		},
	}
}

// ServeConn implements Handler.
func (h *RTMPHandler) ServeConn(ctx context.Context, conn net.Conn, _ Protocol) {
	if err := h.server.Serve(ctx, conn); err != nil {
		h.server.Logger.Debug("rtmp session ended", "remote", conn.RemoteAddr().String(), "err", err)
	}
}

type engineSinkFactory struct {
	engine   IngestEngine
	newFLV   func(string) (rtmp.FLVSink, error)
	newAudio func(string, *webrtc.IngestSession) (rtmp.FLVSink, error)
	log      *slog.Logger
}

func (f *engineSinkFactory) Open(streamKey string) (rtmp.MediaSink, error) {
	session, err := f.engine.Ingest(streamKey, "rtmp:"+streamKey)
	if err != nil {
		return nil, err
	}
	sink := &engineSink{session: session}
	if f.newFLV != nil {
		if extra, err := f.newFLV(streamKey); err != nil {
			f.log.Warn("flv sink unavailable", "stream", streamKey, "err", err)
		} else if extra != nil {
			sink.extras = append(sink.extras, extra)
		}
	}
	if f.newAudio != nil {
		if extra, err := f.newAudio(streamKey, session); err != nil {
			f.log.Warn("audio sink unavailable", "stream", streamKey, "err", err)
		} else if extra != nil {
			sink.extras = append(sink.extras, extra)
		}
	}
	f.log.Info("rtmp ingest session opened", "stream", streamKey)
	return sink, nil
}

// engineSink fans demuxed media to the WebRTC engine and, optionally, raw FLV
// tags to additional sinks (transcoder, recorder, audio transcode).
type engineSink struct {
	session *webrtc.IngestSession
	extras  []rtmp.FLVSink
}

func (s *engineSink) SetVideoConfig(sps, pps []byte) error {
	return s.session.SetVideoConfig(sps, pps)
}

func (s *engineSink) WriteVideo(nalus [][]byte, ptsMillis uint32, keyframe bool) error {
	return s.session.WriteVideo(nalus, ptsMillis, keyframe)
}

func (s *engineSink) WriteFLVTag(tagType uint8, timestamp uint32, data []byte) error {
	for _, extra := range s.extras {
		if err := extra.WriteFLVTag(tagType, timestamp, data); err != nil {
			return err
		}
	}
	return nil
}

func (s *engineSink) Close() {
	s.session.Close()
	for _, extra := range s.extras {
		extra.Close()
	}
}
