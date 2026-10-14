package p2p

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func dial(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func readMsg(t *testing.T, c *websocket.Conn) Message {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	var m Message
	if err := c.ReadJSON(&m); err != nil {
		t.Fatalf("read: %v", err)
	}
	return m
}

func writeMsg(t *testing.T, c *websocket.Conn, m Message) {
	t.Helper()
	if err := c.WriteJSON(m); err != nil {
		t.Fatal(err)
	}
}

func TestHubPeerDiscoveryAndRelay(t *testing.T) {
	hub := NewHub(nil, nil, 4)
	mux := http.NewServeMux()
	mux.HandleFunc("/p2p/ws", hub.HandleWS)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/p2p/ws"

	a := dial(t, wsURL)
	defer a.Close()
	writeMsg(t, a, Message{Type: TypeJoin, Channel: "demo"})
	ja := readMsg(t, a)
	if ja.Type != TypeJoined || ja.PeerID == "" {
		t.Fatalf("A joined = %+v", ja)
	}
	if len(ja.Peers) != 0 {
		t.Fatalf("A peers = %v, want none", ja.Peers)
	}

	b := dial(t, wsURL)
	defer b.Close()
	writeMsg(t, b, Message{Type: TypeJoin, Channel: "demo"})
	jb := readMsg(t, b)
	if jb.Type != TypeJoined || len(jb.Peers) != 1 || jb.Peers[0] != ja.PeerID {
		t.Fatalf("B joined = %+v", jb)
	}

	if pj := readMsg(t, a); pj.Type != TypePeerJoined || pj.PeerID != jb.PeerID {
		t.Fatalf("A peer-joined = %+v", pj)
	}

	// A announces a segment; B queries for it.
	writeMsg(t, a, Message{Type: TypeHave, Segments: []string{"seg1"}})
	time.Sleep(150 * time.Millisecond)
	writeMsg(t, b, Message{Type: TypeWant, Segment: "seg1"})
	peers := readMsg(t, b)
	if peers.Type != TypePeers || peers.Segment != "seg1" || len(peers.Peers) != 1 || peers.Peers[0] != ja.PeerID {
		t.Fatalf("B peers = %+v", peers)
	}

	// WebRTC signaling relay A -> B.
	writeMsg(t, a, Message{Type: TypeSignal, To: jb.PeerID, SDP: "offer-sdp", SDPType: "offer"})
	sig := readMsg(t, b)
	if sig.Type != TypeSignal || sig.From != ja.PeerID || sig.SDP != "offer-sdp" || sig.SDPType != "offer" {
		t.Fatalf("B signal = %+v", sig)
	}

	// B leaving notifies A.
	_ = b.Close()
	left := readMsg(t, a)
	if left.Type != TypePeerLeft || left.PeerID != jb.PeerID {
		t.Fatalf("A peer-left = %+v", left)
	}
}

func TestHubJoinRequiresChannel(t *testing.T) {
	hub := NewHub(nil, nil, 4)
	mux := http.NewServeMux()
	mux.HandleFunc("/p2p/ws", hub.HandleWS)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/p2p/ws"

	c := dial(t, wsURL)
	defer c.Close()
	writeMsg(t, c, Message{Type: TypeJoin})
	m := readMsg(t, c)
	if m.Type != "error" {
		t.Fatalf("expected error, got %+v", m)
	}
}
