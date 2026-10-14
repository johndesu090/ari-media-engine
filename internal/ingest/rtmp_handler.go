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

// RTMPHandler adapts RTMP publisher connections onto the WebRTC ingest pipeline.
type RTMPHandler struct {
	server *rtmp.Server
}

// NewRTMPHandler builds a Handler that serves RTMP publishers into engine.
func NewRTMPHandler(engine IngestEngine, log *slog.Logger, hooks RTMPHooks) *RTMPHandler {
	if log == nil {
		log = slog.Default()
	}
	return &RTMPHandler{
		server: &rtmp.Server{
			Sinks:       &engineSinkFactory{engine: engine, log: log},
			Logger:      log,
			OnPublish:   hooks.OnPublish,
			OnUnpublish: hooks.OnUnpublish,
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
	engine IngestEngine
	log    *slog.Logger
}

func (f *engineSinkFactory) Open(streamKey string) (rtmp.MediaSink, error) {
	session, err := f.engine.Ingest(streamKey, "rtmp:"+streamKey)
	if err != nil {
		return nil, err
	}
	f.log.Info("rtmp ingest session opened", "stream", streamKey)
	return &engineSink{session: session}, nil
}

type engineSink struct {
	session *webrtc.IngestSession
}

func (s *engineSink) SetVideoConfig(sps, pps []byte) error {
	return s.session.SetVideoConfig(sps, pps)
}

func (s *engineSink) WriteVideo(nalus [][]byte, ptsMillis uint32, keyframe bool) error {
	return s.session.WriteVideo(nalus, ptsMillis, keyframe)
}

func (s *engineSink) Close() { s.session.Close() }
