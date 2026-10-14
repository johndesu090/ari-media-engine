package webrtc

import (
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"
)

// TestPublishTrickleICE verifies that a publisher can connect to the engine by
// exchanging ICE candidates incrementally (trickle) rather than waiting for
// gathering to complete.
func TestPublishTrickleICE(t *testing.T) {
	engine, err := NewEngine(Config{EnableLoopback: true}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Shutdown()

	media := &pion.MediaEngine{}
	if err := media.RegisterDefaultCodecs(); err != nil {
		t.Fatal(err)
	}
	pub, err := pion.NewAPI(pion.WithMediaEngine(media)).NewPeerConnection(pion.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()

	track, err := pion.NewTrackLocalStaticSample(pion.RTPCodecCapability{MimeType: pion.MimeTypeVP8}, "v", "s")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pub.AddTrack(track); err != nil {
		t.Fatal(err)
	}

	offer, err := pub.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := pub.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var pubCands []pion.ICECandidateInit
	pub.OnICECandidate(func(c *pion.ICECandidate) {
		if c == nil {
			return
		}
		mu.Lock()
		pubCands = append(pubCands, c.ToJSON())
		mu.Unlock()
	})

	var engineCands []ICECandidate
	answerSDP, err := engine.Publish("demo", "pub-1", pub.LocalDescription().SDP, func(c ICECandidate) {
		mu.Lock()
		engineCands = append(engineCands, c)
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if answerSDP == "" {
		t.Fatal("empty answer SDP")
	}
	if err := pub.SetRemoteDescription(pion.SessionDescription{Type: pion.SDPTypeAnswer, SDP: answerSDP}); err != nil {
		t.Fatal(err)
	}

	// Relay candidates both ways until the publisher reports Connected.
	appliedEngine, appliedPub := 0, 0
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		ec := append([]ICECandidate(nil), engineCands...)
		pc := append([]pion.ICECandidateInit(nil), pubCands...)
		mu.Unlock()

		for ; appliedPub < len(pc); appliedPub++ {
			if err := engine.AddICECandidate("demo", "pub-1", ICECandidate{
				Candidate: pc[appliedPub].Candidate, SDPMid: pc[appliedPub].SDPMid, SDPMLineIndex: pc[appliedPub].SDPMLineIndex,
			}); err != nil {
				t.Fatalf("engine.AddICECandidate: %v", err)
			}
		}
		for ; appliedEngine < len(ec); appliedEngine++ {
			_ = pub.AddICECandidate(pion.ICECandidateInit{
				Candidate: ec[appliedEngine].Candidate, SDPMid: ec[appliedEngine].SDPMid, SDPMLineIndex: ec[appliedEngine].SDPMLineIndex,
			})
		}

		if pub.ConnectionState() == pion.PeerConnectionStateConnected {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("publisher did not connect via trickle ICE; state = %s", pub.ConnectionState())
}
