// Package broadcast defines the core live-stream domain model and its storage
// abstractions. A Broadcast is the unit that Ant Media Server calls an
// "application stream": a named, addressable stream that may be published by a
// single publisher and watched by many subscribers.
package broadcast

import (
	"sync"
	"time"
)

// Type identifies the ingest/playback family of a broadcast.
type Type string

const (
	// TypeWebRTC is a native WebRTC publish/subscribe stream.
	TypeWebRTC Type = "webrtc"
	// TypeRTMP is a stream ingested over RTMP.
	TypeRTMP Type = "rtmp"
	// TypeSRT is a stream ingested over SRT.
	TypeSRT Type = "srt"
	// TypeRTSP is a stream ingested over RTSP.
	TypeRTSP Type = "rtsp"
)

// Status is the lifecycle state of a broadcast.
type Status string

const (
	// StatusCreated means the broadcast exists but has no publisher.
	StatusCreated Status = "created"
	// StatusLive means a publisher is actively sending media.
	StatusLive Status = "live"
	// StatusEnded means the publisher disconnected and the stream is over.
	StatusEnded Status = "ended"
	// StatusError means the broadcast failed.
	StatusError Status = "error"
)

// Delivery selects which playback protocols a stream exposes.
type Delivery string

const (
	// DeliveryAll exposes WebRTC, HLS and DASH.
	DeliveryAll Delivery = "all"
	// DeliveryWebRTC exposes WebRTC only.
	DeliveryWebRTC Delivery = "webrtc"
	// DeliveryHLS exposes HLS only.
	DeliveryHLS Delivery = "hls"
	// DeliveryDASH exposes DASH only.
	DeliveryDASH Delivery = "dash"
)

// Valid reports whether the delivery mode is recognised.
func (d Delivery) Valid() bool {
	switch d {
	case DeliveryAll, DeliveryWebRTC, DeliveryHLS, DeliveryDASH:
		return true
	default:
		return false
	}
}

// UsesWebRTC reports whether the mode includes WebRTC playback.
func (d Delivery) UsesWebRTC() bool { return d == DeliveryAll || d == DeliveryWebRTC }

// UsesHLS reports whether the mode includes HLS playback.
func (d Delivery) UsesHLS() bool { return d == DeliveryAll || d == DeliveryHLS }

// UsesDASH reports whether the mode includes DASH playback.
func (d Delivery) UsesDASH() bool { return d == DeliveryAll || d == DeliveryDASH }

// Snapshot is an immutable, JSON-friendly view of a Broadcast.
type Snapshot struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	StreamID    string            `json:"streamId"`
	Type        Type              `json:"type"`
	Status      Status            `json:"status"`
	Delivery    Delivery          `json:"delivery"`
	CreatedAt   time.Time         `json:"createdAt"`
	StartedAt   *time.Time        `json:"startedAt,omitempty"`
	EndedAt     *time.Time        `json:"endedAt,omitempty"`
	ViewerCount int               `json:"viewerCount"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// Broadcast is a concurrency-safe live stream record.
type Broadcast struct {
	mu sync.RWMutex

	id        string
	name      string
	streamID  string
	btype     Type
	status    Status
	delivery  Delivery
	createdAt time.Time
	startedAt *time.Time
	endedAt   *time.Time
	viewers   int
	metadata  map[string]string
}

// New constructs a broadcast in the created state.
func New(id, streamID, name string, btype Type) *Broadcast {
	return &Broadcast{
		id:        id,
		name:      name,
		streamID:  streamID,
		btype:     btype,
		status:    StatusCreated,
		delivery:  DeliveryAll,
		createdAt: time.Now().UTC(),
		metadata:  map[string]string{},
	}
}

// ID returns the broadcast identifier.
func (b *Broadcast) ID() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.id
}

// StreamID returns the stream key used by publishers and subscribers.
func (b *Broadcast) StreamID() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.streamID
}

// Status returns the current lifecycle status.
func (b *Broadcast) Status() Status {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.status
}

// SetStatus transitions the broadcast to the given status, maintaining the
// started/ended timestamps.
func (b *Broadcast) SetStatus(status Status) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.status = status
	now := time.Now().UTC()
	switch status {
	case StatusLive:
		if b.startedAt == nil {
			b.startedAt = &now
		}
		b.endedAt = nil
	case StatusEnded, StatusError:
		b.endedAt = &now
	}
}

// AddViewer increments the live viewer counter.
func (b *Broadcast) AddViewer() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.viewers++
}

// RemoveViewer decrements the live viewer counter, never going below zero.
func (b *Broadcast) RemoveViewer() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.viewers > 0 {
		b.viewers--
	}
}

// ViewerCount returns the current number of subscribers.
func (b *Broadcast) ViewerCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.viewers
}

// Delivery returns the configured playback delivery mode.
func (b *Broadcast) Delivery() Delivery {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.delivery == "" {
		return DeliveryAll
	}
	return b.delivery
}

// SetDelivery updates the playback delivery mode.
func (b *Broadcast) SetDelivery(d Delivery) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.delivery = d
}

// SetMetadata replaces the broadcast metadata map.
func (b *Broadcast) SetMetadata(meta map[string]string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if meta == nil {
		meta = map[string]string{}
	}
	b.metadata = meta
}

// Snapshot returns an immutable copy of the broadcast state.
func (b *Broadcast) Snapshot() Snapshot {
	b.mu.RLock()
	defer b.mu.RUnlock()

	meta := make(map[string]string, len(b.metadata))
	for k, v := range b.metadata {
		meta[k] = v
	}
	return Snapshot{
		ID:          b.id,
		Name:        b.name,
		StreamID:    b.streamID,
		Type:        b.btype,
		Status:      b.status,
		Delivery:    b.delivery,
		CreatedAt:   b.createdAt,
		StartedAt:   b.startedAt,
		EndedAt:     b.endedAt,
		ViewerCount: b.viewers,
		Metadata:    meta,
	}
}
