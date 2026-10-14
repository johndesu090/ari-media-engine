# Ari Media Engine

An open-source, high-performance, ultra-low-latency live streaming engine that
mirrors and extends the core enterprise features of Ant Media Server.

- **Ultra-low latency:** WebRTC SFU with sub-second (target < 0.5 s) glass-to-glass delivery.
- **Multi-protocol ingest:** WebRTC publish, plus RTMP / SRT / RTSP listeners.
- **Multi-protocol playback:** WebRTC first, with HLS / DASH fallback.
- **Adaptive bitrate (ABR):** on-the-fly transcoding pipeline (FFmpeg/GPU) — see roadmap.
- **Iframe embedding security:** strict CSP `frame-ancestors` plus a server-side origin guard, configurable globally or per stream.
- **Built-in HTML5 player:** self-hosted, embeddable, zero-dependency, with automatic protocol selection.
- **Control plane:** REST API, WebSocket signaling, lifecycle webhooks, and a management dashboard.
- **Horizontal clustering:** Redis-backed node discovery and state (roadmap).

> **Status:** Phase 1 MVP. The WebRTC SFU, signaling, REST control plane,
> embedding security, player and dashboard are functional. RTMP/SRT/RTSP media
> demuxing, ABR transcoding and Redis clustering are scaffolded with clear
> extension points (see [`docs/architecture.md`](docs/architecture.md)).

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
| `auth.enabled` / `auth.api_key`      | `ARI_AUTH_ENABLED`/`ARI_API_KEY`   | API key protection for control-plane routes |

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

   - **Browser publisher:** open `/player.html?id=demo` for playback and use any
     WebRTC publish client against `ws://localhost:8080/ws`.
   - **OBS / RTMP:** server `rtmp://localhost:1935/live`, stream key `demo`
     (media demuxing is on the roadmap; the listener accepts connections today).

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

## REST API

| Method | Path                                        | Description |
| ------ | ------------------------------------------- | ----------- |
| GET    | `/healthz`                                  | Liveness probe |
| GET    | `/api/v1/stats`                             | Aggregate node stats |
| GET    | `/api/v1/streams`                           | List streams |
| POST   | `/api/v1/streams`                           | Create a stream |
| GET    | `/api/v1/streams/{id}`                      | Get a stream |
| DELETE | `/api/v1/streams/{id}`                      | Delete a stream |
| GET    | `/api/v1/streams/{id}/stats`                | Per-stream stats + effective embed policy |
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
internal/ingest        RTMP / SRT / RTSP listeners
internal/webrtc        Pion-based SFU
internal/transcoding   ABR pipeline (roadmap)
internal/delivery      Embedding security (domain policy + CSP middleware)
internal/player        Built-in HTML5 player + embedded assets
internal/storage       Local / S3 recording storage (roadmap)
internal/cluster       Redis-backed clustering (roadmap)
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
