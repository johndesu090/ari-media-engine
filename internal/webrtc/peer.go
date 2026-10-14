package webrtc

import pion "github.com/pion/webrtc/v4"

// peerRole distinguishes publishers from subscribers.
type peerRole string

const (
	rolePublisher  peerRole = "publisher"
	roleSubscriber peerRole = "subscriber"
)

// peer is a single WebRTC session (one PeerConnection).
type peer struct {
	id   string
	role peerRole
	pc   *pion.PeerConnection
}

// close tears down the underlying PeerConnection.
func (p *peer) close() {
	if p != nil && p.pc != nil {
		_ = p.pc.Close()
	}
}
