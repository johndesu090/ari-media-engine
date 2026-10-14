package webrtc

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/pion/rtp"
	pion "github.com/pion/webrtc/v4"
)

// h264MTU bounds the RTP payload so packets stay under typical path MTU.
const h264MTU = 1200

// h264DefaultFmtp matches the first H.264 codec registered by Pion's
// RegisterDefaultCodecs so that the ingest track negotiates cleanly. The real
// stream profile is conveyed in-band through SPS/PPS (STAP-A).
const h264DefaultFmtp = "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f"

// IngestSession publishes externally demuxed media (for example RTMP) into a
// stream without a WebRTC publisher. Video is H.264; audio transcoding
// (AAC→Opus) is provided by the transcode pipeline.
type IngestSession struct {
	engine    *Engine
	streamID  string
	sessionID string

	mu     sync.Mutex
	video  *pion.TrackLocalStaticRTP
	audio  *pion.TrackLocalStaticRTP
	sps    []byte
	pps    []byte
	seq    uint16
	ts     uint32
	closed bool

	audioPackets atomic.Uint64
}

// Ingest opens a media-ingest session for streamID. It fails when the stream
// already has a publisher or another ingest session.
func (e *Engine) Ingest(streamID, sessionID string) (*IngestSession, error) {
	s := e.getOrCreateStream(streamID)

	s.mu.Lock()
	busy := s.publisher != nil || s.ingest != nil
	if !busy {
		session := &IngestSession{engine: e, streamID: streamID, sessionID: sessionID}
		s.ingest = session
		s.mu.Unlock()
		return session, nil
	}
	s.mu.Unlock()
	return nil, ErrBroadcastLive
}

// SetVideoConfig records the H.264 SPS/PPS and creates the outbound video track.
func (s *IngestSession) SetVideoConfig(sps, pps []byte) error {
	s.mu.Lock()
	if s.closed || s.video != nil || len(sps) == 0 || len(pps) == 0 {
		s.mu.Unlock()
		return nil
	}
	track, err := pion.NewTrackLocalStaticRTP(
		pion.RTPCodecCapability{MimeType: pion.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: h264DefaultFmtp},
		s.sessionID, s.streamID,
	)
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("webrtc: create ingest track: %w", err)
	}
	s.sps = append([]byte(nil), sps...)
	s.pps = append([]byte(nil), pps...)
	s.video = track
	s.mu.Unlock()

	if st := s.stream(); st != nil {
		st.addTrack(track)
		st.scheduleLive()
	}
	return nil
}

// WriteVideo packetizes an H.264 access unit and fans it out to subscribers.
// ptsMillis is the RTMP timestamp in milliseconds.
func (s *IngestSession) WriteVideo(nalus [][]byte, ptsMillis uint32, keyframe bool) error {
	s.mu.Lock()
	if s.closed || s.video == nil {
		s.mu.Unlock()
		return nil
	}
	s.ts = ptsMillis * 90 // 90 kHz H.264 clock

	var packets []*rtp.Packet
	if keyframe && len(s.sps) > 0 && len(s.pps) > 0 {
		packets = append(packets, stapAPacket(s.sps, s.pps, &s.seq, s.ts))
	}
	packets = append(packets, packetizeH264(nalus, &s.seq, s.ts)...)
	video := s.video
	s.mu.Unlock()

	for _, p := range packets {
		if err := video.WriteRTP(p); err != nil {
			return err
		}
	}
	return nil
}

// EnsureAudioTrack creates and registers an Opus audio track for the session
// (idempotent). RTMP AAC audio is transcoded to Opus by the audio pipeline and
// forwarded here.
func (s *IngestSession) EnsureAudioTrack() (*pion.TrackLocalStaticRTP, error) {
	s.mu.Lock()
	if s.audio != nil {
		track := s.audio
		s.mu.Unlock()
		return track, nil
	}
	track, err := pion.NewTrackLocalStaticRTP(
		pion.RTPCodecCapability{MimeType: pion.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		s.sessionID+"-audio", s.streamID,
	)
	if err != nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("webrtc: create audio track: %w", err)
	}
	s.audio = track
	s.mu.Unlock()

	if st := s.stream(); st != nil {
		st.addTrack(track)
		st.scheduleLive()
	}
	return track, nil
}

// WriteAudioRTP forwards an Opus RTP packet to subscribers.
func (s *IngestSession) WriteAudioRTP(pkt *rtp.Packet) error {
	s.mu.Lock()
	track := s.audio
	s.mu.Unlock()
	if track == nil {
		return nil
	}
	s.audioPackets.Add(1)
	return track.WriteRTP(pkt)
}

// AudioPackets returns the number of Opus RTP packets forwarded to subscribers.
func (s *IngestSession) AudioPackets() uint64 { return s.audioPackets.Load() }

// Close ends the ingest session and tears down its track.
func (s *IngestSession) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.video = nil
	s.audio = nil
	s.mu.Unlock()

	if st := s.stream(); st != nil {
		st.removeIngest(s)
	}
}

func (s *IngestSession) stream() *stream {
	s.engine.mu.RLock()
	defer s.engine.mu.RUnlock()
	return s.engine.streams[s.streamID]
}

// packetizeH264 splits NAL units into RTP packets per RFC 6184, marking the
// final packet of the access unit.
func packetizeH264(nalus [][]byte, seq *uint16, ts uint32) []*rtp.Packet {
	var packets []*rtp.Packet
	for _, nalu := range nalus {
		if len(nalu) == 0 {
			continue
		}
		packets = append(packets, packetizeNALU(nalu, seq, ts)...)
	}
	if len(packets) > 0 {
		packets[len(packets)-1].Marker = true
	}
	return packets
}

func packetizeNALU(nalu []byte, seq *uint16, ts uint32) []*rtp.Packet {
	if len(nalu) <= h264MTU {
		p := &rtp.Packet{
			Header:  rtp.Header{Version: 2, SequenceNumber: *seq, Timestamp: ts},
			Payload: append([]byte(nil), nalu...),
		}
		*seq++
		return []*rtp.Packet{p}
	}

	// Fragmentation unit (FU-A).
	indicator := (nalu[0] & 0xe0) | 28
	typ := nalu[0] & 0x1f
	body := nalu[1:]
	maxFrag := h264MTU - 2

	var out []*rtp.Packet
	for offset := 0; offset < len(body); {
		n := maxFrag
		if len(body)-offset < n {
			n = len(body) - offset
		}
		start := offset == 0
		end := offset+n >= len(body)

		fuHeader := typ
		if start {
			fuHeader |= 0x80
		}
		if end {
			fuHeader |= 0x40
		}
		payload := make([]byte, 2+n)
		payload[0] = indicator
		payload[1] = fuHeader
		copy(payload[2:], body[offset:offset+n])

		out = append(out, &rtp.Packet{
			Header:  rtp.Header{Version: 2, SequenceNumber: *seq, Timestamp: ts},
			Payload: payload,
		})
		*seq++
		offset += n
	}
	return out
}

// stapAPacket builds an STAP-A packet carrying SPS and PPS.
func stapAPacket(sps, pps []byte, seq *uint16, ts uint32) *rtp.Packet {
	payload := []byte{(sps[0] & 0xe0) | 24} // STAP-A
	for _, nalu := range [][]byte{sps, pps} {
		payload = append(payload, byte(len(nalu)>>8), byte(len(nalu)))
		payload = append(payload, nalu...)
	}
	p := &rtp.Packet{
		Header:  rtp.Header{Version: 2, SequenceNumber: *seq, Timestamp: ts},
		Payload: payload,
	}
	*seq++
	return p
}
