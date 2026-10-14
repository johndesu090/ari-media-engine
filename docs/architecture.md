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
| `internal/ingest`    | RTMP demux (handshake, chunks, AMF0, FLV → H.264) and SRT/RTSP listeners |
| `internal/delivery`  | Embedding security: domain allow-list + CSP `frame-ancestors` + origin guard |
| `internal/player`    | Embedded HTML5 player page and assets |
| `internal/api`       | REST control plane (streams, security, stats) |
| `internal/transcode` | FFmpeg ABR ladder: pipes FLV to FFmpeg and serves the HLS renditions |
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

## RTMP ingest path

`internal/ingest/rtmp` implements the publisher side of RTMP:

1. **Handshake** — the RTMP simple handshake (C0/C1/S0/S1/S2/C2).
2. **Chunk streams** — basic + message header parsing with format 0–3 chunks,
   extended timestamps, chunk-size changes and multi-chunk reassembly.
3. **AMF0** — decoding of `connect`, `createStream` and `publish`, and encoding
   of `_result`/`onStatus` replies.
4. **FLV demux** — video tags are decoded from the AVC (`AVCDecoderConfiguration
   Record` → SPS/PPS; length-prefixed samples → NAL units).
5. **Fan-out** — `webrtc.IngestSession` creates an H.264 `TrackLocalStaticRTP`,
   sends SPS/PPS in-band via STAP-A before each keyframe, and packetizes access
   units with RFC 6184 single-NALU / FU-A framing. Subscribers then receive the
   stream exactly like a native WebRTC publisher.

Publish lifecycle (`OnPublish`/`OnUnpublish`) updates the control-plane
`Broadcast` record, so RTMP streams appear in the REST API and dashboard.

Remaining in this track: SRT and RTSP demuxing, and AAC→Opus audio transcoding
(WebRTC browsers do not negotiate AAC, so RTMP→WebRTC currently carries H.264
video).

## ABR transcoding

`internal/transcode` runs an FFmpeg-based adaptive-bitrate ladder. When
transcoding is enabled, the RTMP session also forwards every raw FLV tag to a
transcode `Session` (the same `MediaSink` implements the optional `rtmp.FLVSink`
interface). The session:

1. Frames the tags into an FLV byte stream (`flvWriter`) and pipes it to
   `ffmpeg -f flv -i pipe:0`.
2. Splits and scales the video into N renditions, encodes each with libx264 at
   the configured bitrate, and aligns keyframes (`-g`, `-sc_threshold 0`) so
   players can switch cleanly.
3. Muxes an HLS ladder: a master `index.m3u8` plus per-variant playlists and
   `.ts` segments under `<output_dir>/<streamID>/`, served by the engine's
   `/hls/` handler.

Sessions start on RTMP publish and stop on unpublish; a transcode failure never
drops the WebRTC stream (the extra sink is best-effort). The pipeline is
video-only for now — audio muxing and DASH output belong to the HLS/DASH track.

## Delivery modes and tokenized playback

Each stream carries a `Delivery` mode (`all`, `webrtc`, `hls`, `dash`) on the
`Broadcast`. It drives two things:

- **Signaling:** `signaling` refuses `subscribe` when the mode excludes WebRTC.
- **Packaging:** the transcode manager produces HLS and/or DASH accordingly
  (`deliveryFormats`); `webrtc`-only streams are not transcoded at all.

`delivery.TokenSigner` mints stateless HMAC-SHA256 tokens
(`<unixExpiry>.<hexSignature>`) and `delivery.PlaybackServer` enforces them for
`/hls/` and `/dash/`. The token is a URL **path** segment
(`/hls/<token>/<streamID>/…`) so relative playlist and segment requests inherit
it — query strings are not propagated by native HLS or hls.js. The player
handler mints a token server-side, so playback via `/player.html` keeps working,
while direct requests without a valid token receive `401`. The API and dashboard
can also mint tokens for external players.

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

Playback is forced inline on every platform: `playsinline` and
`webkit-playsinline` (iOS/iPadOS), plus `x5-playsinline` and
`x5-video-player-type=h5-page` for Android WebViews; the legacy iOS
`webkitEnterFullscreen` entry point is neutralised. Autoplay starts muted and a
tap-to-play overlay plus first-gesture retry covers strict autoplay policies.
On iOS the volume slider is hidden because programmatic volume is ignored.

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
| RTMP/SRT/RTSP media demux | 🟡 RTMP done | RTMP FLV→H.264→RTP works end-to-end; SRT/RTSP + AAC→Opus pending |
| ABR transcoding | ✅ MVP | FFmpeg FLV→HLS ladder, auto start/stop on publish |
| HLS/DASH packaging | ✅ MVP | HLS master+variants and DASH MPD produced by the ABR pipeline (video-only) |
| Delivery modes + tokens | ✅ MVP | Per-stream webrtc/hls/dash/all; HMAC tokenization for HLS/DASH |
| Redis clustering | ⏳ | `Store` interface ready for a Redis implementation |
| Recording + S3 sync | ⏳ | Config present; recorder pending |
| Trickle ICE / simulcast | ⏳ | Reduces setup latency, improves large-scale fan-out |
