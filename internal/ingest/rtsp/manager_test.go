package rtsp

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"

	"github.com/johndesu090/ari-media-engine/internal/webrtc"
)

// testRTSPServer is a minimal RTSP server (interleaved RTP) used to exercise the
// pull client without external tooling.
type testRTSPServer struct {
	ln       net.Listener
	sps, pps []byte
}

func newTestRTSPServer(t *testing.T) *testRTSPServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &testRTSPServer{
		ln:  ln,
		sps: []byte{0x67, 0x42, 0xE0, 0x1F, 0xDA},
		pps: []byte{0x68, 0xCE, 0x3C},
	}
	go s.serve()
	return s
}

func (s *testRTSPServer) url() string {
	return "rtsp://" + s.ln.Addr().String() + "/live"
}

func (s *testRTSPServer) close() { _ = s.ln.Close() }

func (s *testRTSPServer) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *testRTSPServer) handle(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	const session = "ARISESSION"
	streaming := false

	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		fields := strings.SplitN(line, " ", 3)
		if len(fields) < 2 {
			return
		}
		method := fields[0]
		cseq := ""
		for {
			h, err := br.ReadString('\n')
			if err != nil {
				return
			}
			h = strings.TrimRight(h, "\r\n")
			if h == "" {
				break
			}
			if strings.HasPrefix(strings.ToLower(h), "cseq:") {
				cseq = strings.TrimSpace(h[5:])
			}
		}

		switch method {
		case "OPTIONS":
			writeResponse(conn, cseq, "Public: OPTIONS, DESCRIBE, SETUP, PLAY, TEARDOWN\r\n", "")
		case "DESCRIBE":
			writeResponse(conn, cseq, "Content-Type: application/sdp\r\n", s.sdp())
		case "SETUP":
			writeResponse(conn, cseq, "Transport: RTP/AVP/TCP;unicast;interleaved=0-1\r\nSession: "+session+"\r\n", "")
		case "PLAY":
			writeResponse(conn, cseq, "Session: "+session+"\r\n", "")
			if !streaming {
				streaming = true
				go s.stream(conn)
			}
		case "TEARDOWN":
			return
		}
	}
}

func (s *testRTSPServer) sdp() string {
	return "v=0\r\n" +
		"o=- 0 0 IN IP4 127.0.0.1\r\n" +
		"s=ari-test\r\n" +
		"t=0 0\r\n" +
		"m=video 0 RTP/AVP 96\r\n" +
		"a=rtpmap:96 H264/90000\r\n" +
		"a=fmtp:96 packetization-mode=1;sprop-parameter-sets=" +
		base64.StdEncoding.EncodeToString(s.sps) + "," + base64.StdEncoding.EncodeToString(s.pps) + "\r\n" +
		"a=control:trackID=0\r\n"
}

func (s *testRTSPServer) stream(conn net.Conn) {
	const ssrc = 0x11223344
	var seq uint16

	send := func(nalu []byte, ts uint32, marker bool) bool {
		pkt := &rtp.Packet{
			Header:  rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: seq, Timestamp: ts, SSRC: ssrc, Marker: marker},
			Payload: nalu,
		}
		seq++
		raw, err := pkt.Marshal()
		if err != nil {
			return false
		}
		hdr := []byte{'$', 0, byte(len(raw) >> 8), byte(len(raw))}
		if _, err := conn.Write(append(hdr, raw...)); err != nil {
			return false
		}
		return true
	}

	// Access unit 1: SPS, PPS, IDR (marker on IDR).
	if !send(s.sps, 0, false) || !send(s.pps, 0, false) || !send([]byte{0x65, 0x01, 0x02, 0x03}, 0, true) {
		return
	}
	// A few P frames.
	for i := 1; i <= 20; i++ {
		if !send([]byte{0x41, 0x09, byte(i)}, uint32(i*3000), true) {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
}

func writeResponse(conn net.Conn, cseq, extra, body string) {
	var b strings.Builder
	b.WriteString("RTSP/1.0 200 OK\r\n")
	if cseq != "" {
		b.WriteString("CSeq: " + cseq + "\r\n")
	}
	b.WriteString(extra)
	if body != "" {
		fmt.Fprintf(&b, "Content-Length: %d\r\n", len(body))
	}
	b.WriteString("\r\n")
	b.WriteString(body)
	_, _ = io.WriteString(conn, b.String())
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestRTSPPull(t *testing.T) {
	srv := newTestRTSPServer(t)
	defer srv.close()

	engine, err := webrtc.NewEngine(webrtc.Config{EnableLoopback: true}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Shutdown()

	var mu sync.Mutex
	var sess *webrtc.IngestSession
	mgr := NewManager(engine, testLogger(), Hooks{
		OnSession: func(_ string, s *webrtc.IngestSession) {
			mu.Lock()
			sess = s
			mu.Unlock()
		},
	})
	defer mgr.StopAll()

	if err := mgr.Start("demo", srv.url()); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		s := sess
		mu.Unlock()
		if s != nil && s.VideoAccessUnits() > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("no H.264 access units were received over RTSP")
}

func TestParseSDPAndDepacketizer(t *testing.T) {
	srv := &testRTSPServer{sps: []byte{1, 2, 3}, pps: []byte{4, 5}}
	md, err := parseSDP(srv.sdp())
	if err != nil {
		t.Fatal(err)
	}
	if md.payloadType != 96 || md.clockRate != 90000 || !strings.EqualFold(md.encoding, "H264") {
		t.Fatalf("media = %+v", md)
	}
	if len(md.sps) != 3 || len(md.pps) != 2 {
		t.Fatalf("sps/pps = %x %x", md.sps, md.pps)
	}
	if resolveControl("rtsp://h/live", md.control) != "rtsp://h/live/trackID=0" {
		t.Fatalf("control = %s", resolveControl("rtsp://h/live", md.control))
	}

	// Single NALU + FU-A reassembly.
	d := &h264Depacketizer{}
	nalus, key := d.push([]byte{0x65, 0x01})
	if len(nalus) != 1 || !key {
		t.Fatalf("single nalu: %v key=%v", nalus, key)
	}
	d.push([]byte{0x7c, 0x85, 0xaa})              // FU-A start, type 5
	d.push([]byte{0x7c, 0x05, 0xbb})              // FU-A middle
	nalus, key = d.push([]byte{0x7c, 0x45, 0xcc}) // FU-A end
	if len(nalus) != 1 || !key || nalus[0][0]&0x1f != 5 {
		t.Fatalf("fu-a reassembly: %v key=%v", nalus, key)
	}
}
