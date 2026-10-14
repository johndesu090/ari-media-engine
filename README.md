# Ari Media Engine

An open-source, high-performance, ultra-low-latency live streaming engine that
mirrors and extends the core enterprise features of Ant Media Server.

- **Ultra-low latency:** WebRTC SFU with sub-second (target < 0.5 s) glass-to-glass delivery over trickle ICE.
- **Simulcast:** accepts a publisher's RID layers and forwards the preferred layer to subscribers.
- **Peer-assisted delivery (P2P):** HLS/DASH viewers share segments over WebRTC data channels, with CDN fallback.
- **Multi-protocol ingest:** WebRTC publish, plus RTMP ingest (H.264) and SRT / RTSP listeners.
- **Multi-protocol playback:** WebRTC first, with HLS / DASH fallback.
- **Per-stream delivery modes:** WebRTC-only, HLS-only, DASH-only or all, switchable from the dashboard or API.
- **Signed playback tokens:** HMAC tokenization for HLS/DASH, toggleable at runtime from the dashboard.
- **Adaptive bitrate (ABR):** on-the-fly FFmpeg transcoding into a multi-rendition HLS/DASH ladder (1080p/720p/480p).
- **Iframe embedding security:** strict CSP `frame-ancestors` plus a server-side origin guard, configurable globally or per stream.
- **Built-in HTML5 player:** self-hosted, embeddable, zero-dependency, with automatic protocol selection.
- **Control plane:** REST API, WebSocket signaling, lifecycle webhooks, and a management dashboard.
- **Horizontal clustering:** Redis-backed node discovery and TTL heartbeats.
- **Recording & storage:** record published streams and sync to local disk or S3/MinIO.

> **Status:** Phase 1 MVP. The WebRTC SFU (with **trickle ICE** and
> **simulcast**), signaling, REST control plane, embedding security, player,
> dashboard, **RTMP ingest**, **ABR transcoding**, **per-stream delivery
> modes**, **HLS/DASH tokenization**, **Redis clustering**, **recording to
> local/S3 storage** and **peer-assisted (P2P) delivery** are functional. RTMP
> publishes are demuxed (FLV → H.264) and fanned out to WebRTC subscribers,
> optionally transcoded into an HLS/DASH ABR ladder served at
> `/hls/<streamID>/index.m3u8` and `/dash/<streamID>/manifest.mpd`, and
> optionally recorded. SRT/RTSP demuxing and AAC→Opus audio transcoding remain
> scaffolded with clear extension points (see
> [`docs/architecture.md`](docs/architecture.md)).

---

## Quickstart with Docker Compose

The compose stack starts the engine alongside Redis (clustering state) and MinIO
(S3-compatible storage).

```bash
docker compose up --build
```

Then open the console at <http://localhost:8080/>.

| Service            | URL / Port                          |
| ------------------ | ----------------------------------- |
| Dashboard          | http://localhost:8080/              |
| REST API           | http://localhost:8080/api/v1/       |
| Player             | http://localhost:8080/player.html?id=STREAM_ID |
| WebSocket signaling| ws://localhost:8080/ws              |
| RTMP ingest        | rtmp://localhost:1935/live/STREAM_ID |
| SRT ingest         | srt://localhost:8890                |
| MinIO console      | http://localhost:9001 (ari / ariminio123) |

## Build and run from source

Requires Go 1.24+.

```bash
go build ./cmd/ari
./ari -config configs/config.yaml
```

Or run directly:

```bash
go run ./cmd/ari -config configs/config.yaml
```

Verify:

```bash
curl http://localhost:8080/healthz
curl http://localhost:8080/api/v1/stats
```

## Configuration

Configuration is YAML (`configs/config.yaml`) with `ARI_*` environment
overrides. Highlights:

| Setting                              | Env var                            | Description |
| ------------------------------------ | ---------------------------------- | ----------- |
| `server.public_url`                  | `ARI_SERVER_HOST`/`PORT`           | Externally reachable base URL used in embed snippets |
| `security.allow_all_embed`           | `ARI_SECURITY_ALLOW_ALL_EMBED`     | Permit framing on any domain |
| `security.allowed_domains`           | `ARI_SECURITY_ALLOWED_DOMAINS`     | Comma-separated global allow-list |
| `security.enforce_origin`            | `ARI_SECURITY_ENFORCE_ORIGIN`      | Reject non-allow-listed `Origin`/`Referer` server-side |
| `player.title`                       | `ARI_PLAYER_TITLE`                 | Player/dashboard title |
| `webhooks.enabled` / `webhooks.urls` | `ARI_WEBHOOKS_ENABLED`/`_URLS`     | Stream lifecycle webhooks |
| `transcode.enabled`                  | `ARI_TRANSCODE_ENABLED`            | Enable the FFmpeg ABR → HLS pipeline |
| `transcode.ffmpeg_path`              | `ARI_TRANSCODE_FFMPEG_PATH`        | ffmpeg executable (default `ffmpeg`) |
| `transcode.output_dir`               | `ARI_TRANSCODE_OUTPUT_DIR`         | HLS output root, served at `/hls` |
| `token.enabled`                      | `ARI_TOKEN_ENABLED`                | Require signed tokens for HLS/DASH |
| `token.secret`                       | `ARI_TOKEN_SECRET`                 | HMAC signing secret |
| `token.ttl_sec`                      | `ARI_TOKEN_TTL_SEC`                | Token lifetime in seconds |
| `player.dash_js_url`                 | `ARI_PLAYER_DASH_JS_URL`           | Optional dash.js build for DASH playback |
| `player.hls_js_url`                  | `ARI_PLAYER_HLS_JS_URL`            | Optional hls.js build for HLS playback |
| `p2p.enabled`                        | `ARI_P2P_ENABLED`                  | Enable peer-assisted delivery |
| `p2p.ws_path`                        | `ARI_P2P_WS_PATH`                  | Tracker WebSocket path |
| `p2p.max_peers`                      | `ARI_P2P_MAX_PEERS`                | Peers offered per join |
| `auth.enabled` / `auth.api_key`      | `ARI_AUTH_ENABLED`/`ARI_API_KEY`   | API key protection for control-plane routes |
| `cluster.enabled`                    | `ARI_CLUSTER_ENABLED`              | Join a Redis-backed cluster |
| `cluster.redis_url`                  | `ARI_REDIS_URL`                    | Redis connection URL |
| `cluster.node_id`                    | `ARI_NODE_ID`                      | Node identifier (generated when empty) |
| `recording.enabled`                  | `ARI_RECORDING_ENABLED`            | Record published streams |
| `recording.auto_record`              | `ARI_RECORDING_AUTO`               | Record automatically on publish |
| `storage.driver`                     | `ARI_STORAGE_DRIVER`               | `local` or `s3` |
| `storage.public_base`                | `ARI_STORAGE_PUBLIC_BASE`          | URL prefix for recording links |

## Configuring allowed embedding domains

Embedding is restricted by two layers:

1. **`Content-Security-Policy: frame-ancestors`** — the browser refuses to render
   the player inside a disallowed parent page.
2. **Server-side origin guard** on `/player.html` — returns `403` when the
   `Origin`/`Referer` is not allow-listed (defense in depth).

Set a global allow-list:

```bash
curl -X PUT http://localhost:8080/api/v1/security/domains \
  -H 'Content-Type: application/json' \
  -d '{"allowedDomains":["example.com","*.trusted-partner.net"],"allowAll":false}'
```

Override per stream:

```bash
curl -X PUT http://localhost:8080/api/v1/security/streams/STREAM_ID/domains \
  -H 'Content-Type: application/json' \
  -d '{"allowedDomains":["only-this-site.com"]}'
```

Entries may be bare hosts (`example.com`), full origins
(`https://example.com`) or subdomain wildcards (`*.partner.net`). An empty
per-stream list clears the override and falls back to the global policy.

## Testing the video player

1. Create a stream:

   ```bash
   curl -X POST http://localhost:8080/api/v1/streams \
     -H 'Content-Type: application/json' \
     -d '{"streamId":"demo","name":"Demo","type":"webrtc"}'
   ```

2. Publish to it. The player page and signaling endpoint share the same origin:

   - **WebRTC publisher:** use any WebRTC publish client against
     `ws://localhost:8080/ws` (see the signaling protocol below).
   - **OBS / RTMP:** server `rtmp://localhost:1935/live`, stream key `demo`.
     RTMP publishers are demuxed (FLV → H.264) and fanned out to WebRTC
     subscribers, and appear in the API/dashboard automatically.
   - **ffmpeg (one-liner test):**

     ```bash
     ffmpeg -re -f lavfi -i testsrc=size=640x480:rate=30 \
       -c:v libx264 -preset ultrafast -tune zerolatency \
       -f flv rtmp://localhost:1935/live/demo
     ```

     > RTMP audio (AAC) is parsed but not yet forwarded: browsers require Opus
     > over WebRTC, so AAC→Opus transcoding is part of the next roadmap track.
     > RTMP→WebRTC currently delivers H.264 video.

3. Watch: open <http://localhost:8080/player.html?id=demo>. The player selects
   WebRTC automatically and falls back to HLS if WebRTC is blocked.

4. Embed on an allow-listed site:

   ```html
   <iframe src="http://localhost:8080/player.html?id=demo"
           width="960" height="540" frameborder="0"
           allow="autoplay"></iframe>
   ```

   Loading the same embed from a **non** allow-listed origin is blocked by CSP
   and returns `403` from the guard.

### Inline playback and platform support

The player always plays inline and never takes over the screen as fullscreen,
on every major platform:

- **iOS / iPadOS (Safari, WebView, Chrome, Firefox):** `playsinline` +
  `webkit-playsinline` are set, and the legacy `webkitEnterFullscreen` entry
  point is neutralised. Autoplay starts muted (as iOS requires) and a
  tap-to-play overlay appears if the browser still blocks it.
- **Android (Chrome, Samsung Internet, WebView, Tencent X5/QQ):** `playsinline`,
  `webkit-playsinline`, `x5-playsinline` and `x5-video-player-type=h5-page` are
  applied to keep playback inside the page.
- **Desktop (Windows, macOS, Linux):** inline is the default; the same
  attributes are harmless.
- **Volume:** iOS ignores programmatic volume, so the volume slider is hidden
  there and the mute toggle remains; other platforms keep the full slider.
- **Autoplay policy:** playback is retried on the first user gesture and a
  `Tap to play` affordance is shown whenever the browser refuses to start.

Embeds should keep `allow="autoplay"` on the `<iframe>` so muted autoplay is
permitted inside the frame.

## Adaptive bitrate (ABR) transcoding

Enable the FFmpeg pipeline to produce an HLS ABR ladder for every ingested
stream:

```yaml
transcode:
  enabled: true
  renditions:
    - { name: 1080p, width: 1920, height: 1080, bitrate: 5000k }
    - { name: 720p,  width: 1280, height: 720,  bitrate: 2800k }
    - { name: 480p,  width: 854,  height: 480,  bitrate: 1200k }
```

On publish, the raw FLV stream is piped to FFmpeg, which writes:

```
/hls/<streamID>/index.m3u8          # master (multi-variant) playlist
/hls/<streamID>/0/stream.m3u8       # variant playlists + .ts segments
/hls/<streamID>/1/stream.m3u8
...
```

The built-in player consumes this automatically as its HLS fallback when WebRTC
is unavailable. Transcode sessions start on publish and stop on unpublish, and
`/api/v1/stats` reports the active count as `transcoding`. Requires `ffmpeg` on
the host (bundled in the Docker image).

## Delivery modes and playback tokens

Every stream has a **delivery mode** that controls which playback protocols it
exposes:

| Mode     | Player behaviour                                             |
| -------- | ------------------------------------------------------------ |
| `all`    | WebRTC first, falling back to HLS, then DASH (default)       |
| `webrtc` | WebRTC only (subscribers using HLS/DASH are rejected)        |
| `hls`    | HLS only (WebRTC signaling is refused; no transcode needed)  |
| `dash`   | DASH only                                                    |

Set it per stream from the dashboard dropdown or the API:

```bash
curl -X PUT http://localhost:8080/api/v1/streams/demo/delivery \
  -H 'Content-Type: application/json' -d '{"delivery":"hls"}'
```

The delivery mode also drives packaging: `webrtc` skips transcoding, `hls`/`dash`
produce only that format, and `all` produces both.

**Tokenization** (HLS/DASH) is a runtime toggle in the dashboard's *Playback
tokens* panel or via the API:

```bash
curl -X PUT http://localhost:8080/api/v1/security/token \
  -H 'Content-Type: application/json' -d '{"enabled":true,"ttlSeconds":3600}'
```

When enabled, playlists and segments require an HMAC token carried in the URL
path (`/hls/<token>/<streamID>/…`), so relative segment requests stay authorized.
Mint one for a stream (the dashboard's **Token** button does the same):

```bash
curl -X POST http://localhost:8080/api/v1/streams/demo/token
```

The built-in player mints tokens automatically, so playback through
`/player.html` keeps working; direct HLS/DASH requests without a valid token get
`401`.

## Clustering

Enable `cluster` to join a Redis-backed cluster. Each node registers itself under
`ari:nodes:<id>` with a TTL refreshed by heartbeats, so crashed nodes expire
automatically.

```yaml
cluster:
  enabled: true
  redis_url: redis://redis:6379/0
  node_id: node-1        # generated when empty
  heartbeat_sec: 5
```

Inspect membership:

```bash
curl http://localhost:8080/api/v1/cluster/nodes
```

`/api/v1/stats` also reports `nodeId` and `nodes`. The docker-compose stack
includes Redis and MinIO for local multi-node testing.

## Recording

Enable recording to capture each published stream's raw FLV and upload it to the
configured storage backend on unpublish:

```yaml
recording:
  enabled: true
  auto_record: true
storage:
  driver: local              # or s3
  local_root: ./data/recordings
  public_base: ""            # e.g. https://media.example.com/recordings
```

For S3/MinIO, set `storage.driver: s3` with `s3_endpoint`, `s3_bucket`,
`s3_access_key`, `s3_secret_key` (and `s3_use_ssl`); the bucket is created on
first use. List and delete recordings via the dashboard or the API:

```bash
curl http://localhost:8080/api/v1/recordings
curl -X DELETE http://localhost:8080/api/v1/recordings/STREAM/FILE.flv
```

With the `local` driver, recordings are served at `/recordings/<key>`.

## Peer-assisted delivery (P2P)

Enable P2P to let viewers of a stream share HLS/DASH segments with each other
over WebRTC data channels, reducing origin/CDN load (SwarmCloud-style). The
built-in player uses it automatically when enabled.

```yaml
p2p:
  enabled: true
  ws_path: /p2p/ws
  max_peers: 4
```

How it works:

- Viewers of a stream join a **channel** on the tracker at `/p2p/ws`.
- The tracker returns a few existing peers; the newcomer opens WebRTC data
  channels to them and they announce which segments they hold.
- When a segment is needed, it is fetched from a peer that has it, otherwise
  from the origin, and then announced to the mesh.
- The player wires this into **hls.js** (via a custom `loader`) and **dash.js**
  (by overriding its `XHRLoader`). Manifests and keys always come from the
  origin.

Toggle it at runtime from the dashboard or the API:

```bash
curl -X PUT http://localhost:8080/api/v1/p2p -H 'Content-Type: application/json' -d '{"enabled":true}'
```

To use P2P for HLS in non-Safari browsers, set `player.hls_js_url` (and
`player.dash_js_url` for DASH) so the player loads the corresponding library.

## REST API

| Method | Path                                        | Description |
| ------ | ------------------------------------------- | ----------- |
| GET    | `/healthz`                                  | Liveness probe |
| GET    | `/api/v1/stats`                             | Aggregate node stats |
| GET    | `/api/v1/cluster/nodes`                     | Cluster membership |
| GET    | `/api/v1/recordings`                        | List recordings |
| DELETE | `/api/v1/recordings/{key...}`               | Delete a recording |
| GET    | `/api/v1/p2p`                               | P2P tracker status |
| PUT    | `/api/v1/p2p`                               | Enable/disable P2P |
| GET    | `/api/v1/streams`                           | List streams |
| POST   | `/api/v1/streams`                           | Create a stream |
| GET    | `/api/v1/streams/{id}`                      | Get a stream |
| DELETE | `/api/v1/streams/{id}`                      | Delete a stream |
| GET    | `/api/v1/streams/{id}/stats`                | Per-stream stats + effective embed policy |
| PUT    | `/api/v1/streams/{id}/delivery`             | Set the playback delivery mode |
| POST   | `/api/v1/streams/{id}/token`                | Mint a signed playback token and tokenized URLs |
| GET    | `/api/v1/security/token`                    | Get tokenization settings |
| PUT    | `/api/v1/security/token`                    | Update tokenization settings |
| GET    | `/api/v1/security/domains`                  | Get global embedding policy |
| PUT    | `/api/v1/security/domains`                  | Set global embedding policy |
| GET    | `/api/v1/security/streams/{id}/domains`     | Get per-stream policy |
| PUT    | `/api/v1/security/streams/{id}/domains`     | Set per-stream policy |

When `auth.enabled` is true, send `X-API-Key: <key>` or
`Authorization: Bearer <key>` on control-plane routes.

## Repository layout

```
cmd/server (cmd/ari)   Main entry point
internal/signaling     WebRTC signaling (WebSocket JSON protocol)
internal/ingest        RTMP demux (FLV → H.264) + SRT/RTSP listeners
internal/webrtc        Pion-based SFU
internal/transcode     FFmpeg ABR → HLS ladder
internal/delivery      Embedding security (domain policy + CSP middleware)
internal/player        Built-in HTML5 player + embedded assets
internal/storage       Local / S3 object storage
internal/recorder      FLV recording + upload to storage
internal/flv           FLV container framing (shared)
internal/p2p           P2P tracker: peer channels + WebRTC signaling relay
web/p2p                Client P2P engine (hls.js/dash.js loaders)
internal/cluster       Redis-backed node discovery + heartbeats
internal/api           REST control plane, webhooks
internal/dashboard     Management console (web/dashboard)
internal/webhook       Lifecycle webhook dispatcher
pkg/id, pkg/logger     Shared helpers
configs/               Sample configuration
docs/                  Architecture and API documentation
```

## Development

```bash
go build ./...   # compile everything
go vet ./...     # static checks
go test ./...    # unit tests
```

## License

MIT — see [`LICENSE`](LICENSE).
