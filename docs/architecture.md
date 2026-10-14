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
  RTSP    ──────►             │        └─► transcode (FFmpeg) ────┼──►  HLS / DASH
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
| `internal/audio`     | AAC→Opus transcode (FFmpeg → Opus RTP) for WebRTC subscribers |
| `internal/ingest/rtsp` | RTSP pull client: SDP, H.264 depacketizer, reconnect manager |
| `internal/delivery`  | Embedding security: domain allow-list + CSP `frame-ancestors` + origin guard |
| `internal/player`    | Embedded HTML5 player page and assets |
| `internal/api`       | REST control plane (streams, security, stats) |
| `internal/transcode` | FFmpeg ABR ladder: pipes FLV to FFmpeg and serves the HLS renditions |
| `internal/recorder`  | Records a stream's FLV and uploads it to storage |
| `internal/storage`   | Object storage abstraction (local disk, S3/MinIO) |
| `internal/flv`       | FLV container framing shared by transcode and recorder |
| `internal/p2p`       | P2P tracker: peer channels, `have`/`want`, WebRTC signaling relay |
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

ICE is trickled: the engine returns its SDP immediately and relays gathered
candidates as `candidate` signaling messages, and the client does the same, so
connections establish without waiting for gathering to complete. A non-trickle
fallback is used when no candidate callback is supplied. SVC and congestion
control are follow-ups.

## Trickle ICE and simulcast

`webrtc.Engine.Publish`/`Subscribe` accept an `onCandidate` callback. When set,
the engine registers `OnICECandidate`, returns its SDP without waiting for
gathering, and the signaling layer forwards each candidate as a `candidate`
message; remote candidates arrive as `candidate` messages and are applied with
`Engine.AddICECandidate`. The player mirrors this with `pc.onicecandidate` and
`pc.addIceCandidate`.

Simulcast: a publisher's offer may carry multiple RID layers. `OnTrack` fires
once per RID; the engine creates one `TrackLocalStaticRTP` per RID and
`trackSnapshot` exposes only the preferred layer (`selectRID` ranks `h > m > l`),
which subscribers receive. Unselected layers are still forwarded into unbound
tracks (a no-op fan-out) so a preferred layer that appears later is already
available; per-subscriber layer switching is a follow-up.

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

Remaining in this track: SRT demuxing.

## RTSP pull ingest

`internal/ingest/rtsp` connects to an RTSP source and pulls H.264 into the SFU.
The client performs OPTIONS/DESCRIBE/SETUP/PLAY over one TCP connection using
interleaved RTP (RFC 2326 §10.12), keeping the session alive with periodic
OPTIONS. A small SDP parser extracts the H.264 payload type, clock rate, control
URL and `sprop-parameter-sets`; an RFC 6184 depacketizer reassembles single NAL
units, STAP-A aggregates and FU-A fragments into access units (grouped by the
RTP marker bit) which are written to the ingest session's video track. The
manager keys sessions by stream id, retries with a 3s backoff, and notifies the
control plane via `OnPublish`/`OnUnpublish` so RTSP streams appear in the API.

Sources come from `ingest.rtsp.sources` or `POST /api/v1/rtsp/pull`.

## Audio path

Browsers do not negotiate AAC, so RTMP→WebRTC audio requires transcoding. When
`audio.enabled` is set, the RTMP session's raw FLV is also forwarded to an
`audio.Session`, which pipes it to FFmpeg (`-vn -c:a libopus`) and directs the
resulting Opus RTP to a loopback UDP port the session reads back. Each RTP
packet is written to an Opus `TrackLocalStaticRTP` created on the ingest
session (`EnsureAudioTrack`), so subscribers receive H.264 video and Opus audio
together. The audio sink is best-effort: a failure never drops the video path.

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

## Recording and storage

When `recording.enabled` is set, the RTMP session's raw FLV tags are also
forwarded (through `rtmp.MultiFLVSink`) to a `recorder.Session`, which frames
them into an FLV file via `internal/flv`. On unpublish the file is uploaded to
the configured `storage.Store` — `LocalStore` writes under `local_root`, while
`S3Store` uses MinIO's S3 client and creates the bucket on first use — and the
working copy is removed. Recordings are listed and deleted via
`/api/v1/recordings` and, for the local driver, served at `/recordings/<key>`.

Because the FLV fan-out feeds both the transcoder and the recorder, a single
publish can be recorded and transcoded into HLS/DASH at the same time.

## Peer-assisted delivery (P2P)

`internal/p2p` is a tracker that groups viewers of a stream into a channel. It
never sees media: it records which segments each peer holds (`have`), answers
`want` queries with candidate peers, and relays `signal` messages so peers can
open RTCDataChannels between themselves. Peers are added with a TTL-free
WebSocket session; a `peer-joined`/`peer-left` broadcast keeps the mesh aware of
membership. The tracker is toggled at runtime via `/api/v1/p2p`.

The client engine (`web/p2p/p2p.js`, served at `/p2p/p2p.js`) connects to the
tracker, forms a small mesh (the newcomer initiates offers to the peers it is
given), and transfers segments over data channels using a compact binary
framing (request / response header / 16 KiB chunks / miss / have). Segments
acquired from a peer or the origin are cached in memory and announced to the
mesh. The engine exposes `hlsLoader(Hls)` (a hls.js loader class) and
`setupDash(player)` (a best-effort dash.js `XHRLoader` override); manifests and
keys always go to the origin. A `fetch` primitive provides CDN fallback.

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

`internal/cluster` provides Redis-backed node discovery. Each node registers a
JSON record under `ari:nodes:<id>` with a TTL refreshed by a heartbeat goroutine
(`cluster.heartbeat_sec`), so crashed nodes expire without an explicit
deregistration. `Manager` reports the local node's stream/viewer counts on each
heartbeat and exposes membership via `GET /api/v1/cluster/nodes`. A
`MemoryRegistry` implements the same interface for single-node development and
tests (exercised with `miniredis`).

`broadcast.Store` remains an in-process interface; the next clustering step is a
Redis-backed `Store` plus cross-node stream ownership and fan-out, using the
cluster registry for discovery. The interface boundary was chosen so the media
path never needs to change.

## Roadmap

| Area | Status | Notes |
| ---- | ------ | ----- |
| WebRTC SFU + signaling | ✅ MVP | Trickle ICE, RTP forwarding, simulcast reception |
| Embed security + player | ✅ MVP | CSP + origin guard + embedded assets |
| REST API + webhooks | ✅ MVP | API key auth, lifecycle events |
| RTMP/SRT/RTSP media demux | 🟡 RTMP + RTSP | RTMP FLV→H.264 + AAC→Opus and RTSP pull (H.264) work; SRT pending |
| ABR transcoding | ✅ MVP | FFmpeg FLV→HLS ladder, auto start/stop on publish |
| HLS/DASH packaging | ✅ MVP | HLS master+variants and DASH MPD produced by the ABR pipeline (video-only) |
| Delivery modes + tokens | ✅ MVP | Per-stream webrtc/hls/dash/all; HMAC tokenization for HLS/DASH |
| Redis clustering | ✅ MVP | Node registry + TTL heartbeats; cross-node stream routing pending |
| Recording + S3 sync | ✅ MVP | Raw FLV recording uploaded to local/S3 storage on unpublish |
| Peer-assisted delivery (P2P) | ✅ MVP | WebRTC data-channel mesh + tracker; hls.js/dash.js loaders with CDN fallback |
| Trickle ICE / simulcast | ✅ MVP | Candidates relayed incrementally; SFU forwards the preferred RID layer |
