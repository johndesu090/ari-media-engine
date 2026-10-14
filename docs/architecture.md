# Ari Media Engine — Architecture

This document describes the Phase 1 MVP architecture and the extension points
for the remaining enterprise features.

## Goals

- Sub-second glass-to-glass latency for live streams.
- One ingest path, many output protocols.
- Strict, administrator-controlled iframe embedding.
- A self-contained, embeddable HTML5 player.
- A clean path to horizontal scaling.

## High-level component map

```
                    ┌──────────────────────────────────────────────┐
  publishers        │                Ari Media Engine               │      viewers
  ──────────        │                                              │      ───────
  WebRTC  ──ws──►  signaling ─┐                                   │
  RTMP    ──────►  ingest     ├─►  webrtc.SFU ──►  WebRTC out  ───┼──►  player.html
  SRT     ──────►  (listener) │        │                          │       (WebRTC)
  RTSP    ──────►             │        └─► transcode (roadmap) ───┼──►  HLS / DASH
                             │                                     │
  operators ──http──►  api (REST) ──► broadcast.Store ──► delivery.DomainPolicy
                              │            │                        │
                              └──► webhook.Dispatcher         player assets
                              └──► dashboard (static)
```

## Packages

| Package              | Responsibility |
| -------------------- | -------------- |
| `internal/config`    | YAML + `ARI_*` env configuration, defaults, validation |
| `internal/broadcast` | Live-stream domain model (`Broadcast`) and `Store` abstraction |
| `internal/webrtc`    | Pion-based SFU: one publisher fanned out to many subscribers |
| `internal/signaling` | WebSocket JSON signaling driving the SFU |
| `internal/ingest`    | RTMP/SRT/RTSP listener lifecycle with pluggable handlers |
| `internal/delivery`  | Embedding security: domain allow-list + CSP `frame-ancestors` + origin guard |
| `internal/player`    | Embedded HTML5 player page and assets |
| `internal/api`       | REST control plane (streams, security, stats) |
| `internal/webhook`   | Asynchronous lifecycle webhook dispatcher |
| `internal/server`    | Composition root: wires every subsystem into one HTTP server |
| `pkg/id`, `pkg/logger` | Identifier generation and structured logging |

## Media path (Phase 1)

1. A publisher opens a WebSocket to `/ws` and sends a `publish` message with an
   SDP offer. `signaling` calls `webrtc.Engine.Publish`, which:
   - creates a `PeerConnection`,
   - on each `OnTrack`, creates a `TrackLocalStaticRTP` and forwards RTP packets
     into it (no transcoding), and
   - returns the SDP answer.
2. Subscribers send `subscribe`; the engine creates a `PeerConnection`, adds the
   publisher's tracks, and returns an SDP offer. The client replies with
   `subscriber-answer`.
3. RTP is fanned out unchanged, so latency is dominated by network RTT.

ICE is non-trickle: the full SDP is exchanged in a single message, which is
simpler and proxy-friendly. Trickle ICE, simulcast/SVC and congestion control
are follow-ups.

## Embedding security

Two independent layers enforce iframe restrictions:

1. **`Content-Security-Policy: frame-ancestors`** (browser-enforced). The
   `delivery.DomainPolicy` computes the source list for a stream and
   `delivery.ApplySecurityHeaders` emits it. This is the authoritative control.
2. **Origin guard** (`delivery.EmbedGuard`). For `/player.html` the middleware
   compares the request `Origin`/`Referer` host against the effective policy and
   returns `403` when it is not allow-listed (`security.enforce_origin`).

Policy resolution is per-stream-override-else-global. Domains may be bare hosts,
full origins, or `*.suffix` wildcards. `allowAll` short-circuits to `*`.

`X-Frame-Options` is emitted only when the policy is expressible by that legacy
header (`DENY`/`SAMEORIGIN`); when an external allow-list is configured it is
omitted so allowed embedders are not blocked.

## Player protocol selection

`player.js` attempts WebRTC first:

```
RTCPeerConnection ─► ws /ws {subscribe} ─► {offer} ─► answer ─► media
        │ failure (no WebRTC, signaling error, negotiation failure)
        ▼
   HLS (hls.js when available, else native) ─► DASH (roadmap)
```

The UI exposes play/pause, mute, volume, a live-status indicator and a
low-latency toggle. The latency readout estimates buffer-behind
(`buffered.end - currentTime`).

## Scaling and state

`broadcast.Store` is an interface with an in-memory implementation today. A
Redis-backed implementation (node registry, heartbeats, stream ownership,
fan-out across nodes) is the intended clustering path; the interface boundary
was chosen so the media path never needs to change.

## Roadmap

| Area | Status | Notes |
| ---- | ------ | ----- |
| WebRTC SFU + signaling | ✅ MVP | Non-trickle ICE, RTP forwarding |
| Embed security + player | ✅ MVP | CSP + origin guard + embedded assets |
| REST API + webhooks | ✅ MVP | API key auth, lifecycle events |
| RTMP/SRT/RTSP media demux | ⏳ | Listener lifecycle present; demux + repacketize to RTP pending |
| ABR transcoding | ⏳ | FFmpeg/GPU pipeline; player already consumes multi-variant HLS |
| HLS/DASH packaging | ⏳ | Output dir + serving wired; segmenter pending |
| Redis clustering | ⏳ | `Store` interface ready for a Redis implementation |
| Recording + S3 sync | ⏳ | Config present; recorder pending |
| Trickle ICE / simulcast | ⏳ | Reduces setup latency, improves large-scale fan-out |
