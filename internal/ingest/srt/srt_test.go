package srt

import (
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	gosrt "github.com/datarhei/gosrt"

	"github.com/johndesu090/ari-media-engine/internal/webrtc"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

/* ------------------------------ TS builders ------------------------------ */

func tsPacket(pid int, pusi bool, cc int, payload []byte) []byte {
	pkt := make([]byte, tsPacketSize)
	for i := range pkt {
		pkt[i] = 0xff
	}
	pkt[0] = 0x47
	b1 := byte((pid >> 8) & 0x1f)
	if pusi {
		b1 |= 0x40
	}
	pkt[1] = b1
	pkt[2] = byte(pid)
	pkt[3] = 0x10 | byte(cc&0x0f)
	copy(pkt[4:], payload)
	return pkt
}

func patPayload(pmtPID int) []byte {
	return []byte{
		0x00,       // pointer_field
		0x00,       // table_id
		0xB0, 0x0D, // section_length
		0x00, 0x01, // transport_stream_id
		0xC1, 0x00, 0x00, // version, section_number, last_section_number
		0x00, 0x01, // program_number
		byte(0xE0 | (pmtPID>>8)&0x1f), byte(pmtPID),
		0x00, 0x00, 0x00, 0x00, // CRC
	}
}

func pmtPayload(videoPID int) []byte {
	return []byte{
		0x00,       // pointer_field
		0x02,       // table_id
		0xB0, 0x12, // section_length
		0x00, 0x01, // program_number
		0xC1, 0x00, 0x00, // version, section_number, last_section_number
		0xE1, 0x00, // PCR_PID
		0xF0, 0x00, // program_info_length
		0x1B, // stream_type H.264
		byte(0xE0 | (videoPID>>8)&0x1f), byte(videoPID),
		0xF0, 0x00, // ES_info_length
		0x00, 0x00, 0x00, 0x00, // CRC
	}
}

func encodePTS(pts uint32) []byte {
	return []byte{
		0x20 | byte((pts>>30)&0x07)<<1 | 0x01,
		byte((pts >> 22) & 0xff),
		byte((pts>>14)&0xfe) | 0x01,
		byte((pts >> 7) & 0xff),
		byte((pts<<1)&0xfe) | 0x01,
	}
}

func pesPayload(es []byte, pts uint32) []byte {
	body := append([]byte{0x80, 0x80, 0x05}, encodePTS(pts)...)
	body = append(body, es...)
	length := len(body)
	out := []byte{0x00, 0x00, 0x01, 0xE0, byte(length >> 8), byte(length)}
	return append(out, body...)
}

func annexB(keyframe bool) []byte {
	es := []byte{
		0x00, 0x00, 0x00, 0x01, 0x67, 0x42, 0xE0, 0x1F, 0xDA, // SPS
		0x00, 0x00, 0x00, 0x01, 0x68, 0xCE, 0x3C, // PPS
	}
	if keyframe {
		es = append(es, 0x00, 0x00, 0x00, 0x01, 0x65, 0x01, 0x02, 0x03)
	} else {
		es = append(es, 0x00, 0x00, 0x00, 0x01, 0x41, 0x09, 0x02)
	}
	return es
}

func buildTS() []byte {
	var out []byte
	out = append(out, tsPacket(0, true, 0, patPayload(0x1000))...)
	out = append(out, tsPacket(0x1000, true, 0, pmtPayload(0x100))...)
	for i := 0; i < 12; i++ {
		out = append(out, tsPacket(0x100, true, i, pesPayload(annexB(i == 0), uint32(i)*3000))...)
	}
	return out
}

/* --------------------------------- test ---------------------------------- */

func TestSRTIngest(t *testing.T) {
	engine, err := webrtc.NewEngine(webrtc.Config{EnableLoopback: true}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Shutdown()

	var mu sync.Mutex
	var sess *webrtc.IngestSession
	mgr := NewManager(engine, "127.0.0.1:0", testLogger(), Hooks{
		OnSession: func(_ string, s *webrtc.IngestSession) {
			mu.Lock()
			sess = s
			mu.Unlock()
		},
	})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()

	cfg := gosrt.DefaultConfig()
	cfg.StreamId = "demo"
	cfg.Logger = discardLogger{}
	conn, err := gosrt.Dial("srt", mgr.Addr(), cfg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	ts := buildTS()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := conn.Write(ts); err != nil {
			t.Fatalf("write: %v", err)
		}
		mu.Lock()
		s := sess
		mu.Unlock()
		if s != nil && s.VideoAccessUnits() > 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("no H.264 access units received over SRT")
}

func TestSplitAnnexB(t *testing.T) {
	data := append(annexB(true), 0x00, 0x00, 0x00, 0x01, 0x41, 0xAA)
	nalus := splitAnnexB(data)
	if len(nalus) != 4 {
		t.Fatalf("nalus = %d, want 4", len(nalus))
	}
	if nalus[0][0]&0x1f != 7 || nalus[1][0]&0x1f != 8 || nalus[2][0]&0x1f != 5 || nalus[3][0]&0x1f != 1 {
		t.Fatalf("nalu types = %d %d %d %d", nalus[0][0]&0x1f, nalus[1][0]&0x1f, nalus[2][0]&0x1f, nalus[3][0]&0x1f)
	}
}

func TestTSDemuxer(t *testing.T) {
	var got int
	var keyframe bool
	d := newTSDemuxer(func(_ [][]byte, _ uint32, kf bool) {
		got++
		if kf {
			keyframe = true
		}
	})
	// Two pushes so the last PES is flushed by the next PUSI.
	d.push(buildTS())
	d.push(buildTS())
	if got == 0 {
		t.Fatal("no access units demuxed")
	}
	if !keyframe {
		t.Fatal("expected a keyframe access unit")
	}
}
