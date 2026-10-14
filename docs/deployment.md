# Deployment

This guide covers production deployment on **Ubuntu 24.04** and with **Docker**.

---

## Ports

| Port                 | Protocol | Purpose                                  |
| -------------------- | -------- | ---------------------------------------- |
| `8080`               | TCP      | HTTP API, dashboard, player, `/ws` signaling, `/p2p/ws` |
| `1935`               | TCP      | RTMP ingest                              |
| `8890`               | UDP+TCP  | SRT ingest                               |
| `8554`               | TCP      | RTSP listener (if enabled)               |
| `7881`               | TCP      | ICE-TCP                                  |
| `50000-50100`        | UDP      | ICE media range (`webrtc.ice_udp_port_*`) |
| `6379`               | TCP      | Redis (clustering, internal only)        |
| `9000` / `9001`      | TCP      | MinIO S3 API / console (dev stack only)  |

> WebRTC media is UDP; keep `webrtc.ice_udp_port_min..max` open and matching the
> value published by Docker.

---

## Production configuration

Start from `configs/config.yaml`. The settings that matter most in production:

```yaml
server:
  public_url: "https://media.example.com"   # used in embed snippets and HLS/DASH URLs
webrtc:
  ice_public_ips: ["203.0.113.10"]          # your public IP(s) for NAT traversal
  ice_tcp_port: 7881
  ice_udp_port_min: 50000
  ice_udp_port_max: 50100
auth:
  enabled: true
  api_key: "change-me"                      # protects control-plane routes
token:
  enabled: true
  secret: "change-me"                       # signs HLS/DASH playback tokens
```

Every value can also be set with an `ARI_*` environment variable (see the README
table), which is convenient for containers.

---

## Ubuntu 24.04

### 1. Prerequisites

```bash
sudo apt-get update
sudo apt-get install -y ffmpeg curl ca-certificates
```

Ari Media Engine requires **Go 1.24+** (Ubuntu 24.04 ships an older Go, so
install the official toolchain):

```bash
cd /tmp
curl -LO https://go.dev/dl/go1.24.0.linux-amd64.tar.gz
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf go1.24.0.linux-amd64.tar.gz
export PATH=$PATH:/usr/local/go/bin
go version
```

### 2. Build

```bash
sudo mkdir -p /opt/ari
cd /opt/ari
sudo git clone https://github.com/johndesu090/ari-media-engine.git src
cd src
sudo /usr/local/go/bin/go build -trimpath -o /usr/local/bin/ari ./cmd/ari
```

### 3. Configure

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin ari
sudo mkdir -p /etc/ari /var/lib/ari /opt/ari/web
sudo cp -r /opt/ari/src/web/* /opt/ari/web/
sudo cp /opt/ari/src/configs/config.yaml /etc/ari/config.yaml
sudo chown -R ari:ari /var/lib/ari
sudo nano /etc/ari/config.yaml   # set public_url, ice_public_ips, auth, token
```

> `web/` (dashboard + P2P engine) is served from disk, so the working directory
> must contain it. The service below sets `WorkingDirectory=/opt/ari`.

### 4. systemd service

Create `/etc/systemd/system/ari.service`:

```ini
[Unit]
Description=Ari Media Engine
After=network-online.target
Wants=network-online.target

[Service]
User=ari
Group=ari
WorkingDirectory=/opt/ari
ExecStart=/usr/local/bin/ari -config /etc/ari/config.yaml
Restart=on-failure
RestartSec=3
LimitNOFILE=65536
Environment=ARI_LOG_LEVEL=info

[Install]
WantedBy=multi-user.target
```

Enable and start:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now ari
sudo systemctl status ari
journalctl -u ari -f
```

### 5. Firewall (ufw)

```bash
sudo ufw allow 8080/tcp
sudo ufw allow 1935/tcp
sudo ufw allow 8890/udp
sudo ufw allow 8890/tcp
sudo ufw allow 7881/tcp
sudo ufw allow 50000:50100/udp
sudo ufw enable
```

### 6. TLS reverse proxy (nginx) — recommended

WebRTC media stays on UDP; only HTTP/WebSocket is proxied. Create
`/etc/nginx/sites-available/ari`:

```nginx
server {
    listen 80;
    server_name media.example.com;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }

    # WebRTC signaling and the P2P tracker are WebSockets.
    location /ws {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_read_timeout 3600s;
    }
    location /p2p/ws {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_read_timeout 3600s;
    }
}
```

Then enable it and add TLS with certbot:

```bash
sudo ln -s /etc/nginx/sites-available/ari /etc/nginx/sites-enabled/ari
sudo nginx -t && sudo systemctl reload nginx
sudo apt-get install -y certbot python3-certbot-nginx
sudo certbot --nginx -d media.example.com
```

Set `server.public_url: https://media.example.com` so players and embeds use TLS
(browsers require a secure context for WebRTC).

### 7. Redis (clustering, optional)

```bash
sudo apt-get install -y redis-server
sudo systemctl enable --now redis-server
```

Enable clustering in `/etc/ari/config.yaml`:

```yaml
cluster:
  enabled: true
  redis_url: redis://127.0.0.1:6379/0
```

### 8. Verify

```bash
curl -s http://127.0.0.1:8080/healthz
curl -s http://127.0.0.1:8080/api/v1/stats
```

Open the dashboard at `http://<host>:8080/` (or your TLS domain).

---

## Docker

The image is multi-stage (static Go build + Debian runtime with `ffmpeg`).

### Build

```bash
git clone https://github.com/johndesu090/ari-media-engine.git
cd ari-media-engine
docker build -t ari-media-engine:latest .
```

### Run (single container, host networking)

Host networking is recommended so WebRTC UDP works without a large port map:

```bash
docker run -d --name ari --restart unless-stopped \
  --network host \
  -e ARI_SERVER_PUBLIC_URL="http://203.0.113.10:8080" \
  -e ARI_ICE_PUBLIC_IPS="203.0.113.10" \
  -v "$PWD/data:/app/data" \
  -v "$PWD/configs/config.yaml:/app/config.yaml:ro" \
  ari-media-engine:latest
```

### Run (bridge networking with explicit ports)

If you cannot use host networking, publish every port:

```bash
docker run -d --name ari --restart unless-stopped \
  -p 8080:8080 \
  -p 1935:1935 \
  -p 8890:8890/tcp -p 8890:8890/udp \
  -p 7881:7881/tcp \
  -p 50000-50100:50000-50100/udp \
  -v "$PWD/data:/app/data" \
  ari-media-engine:latest
```

The published UDP range **must** match `webrtc.ice_udp_port_min/max`.

### Docker Compose (engine + Redis + MinIO)

The repository ships `docker-compose.yml`:

```bash
docker compose up -d --build
docker compose logs -f ari
```

| Service | URL                              |
| ------- | -------------------------------- |
| Ari     | http://localhost:8080/           |
| MinIO   | http://localhost:9001 (`ari` / `ariminio123`) |

Stop and remove:

```bash
docker compose down            # keep volumes
docker compose down -v         # also remove data volumes
```

### Point MinIO at the engine (S3 recording)

```yaml
storage:
  driver: s3
  s3_endpoint: "http://minio:9000"
  s3_bucket: recordings
  s3_access_key: ari
  s3_secret_key: ariminio123
recording:
  enabled: true
```

---

## Updating

```bash
# Ubuntu
cd /opt/ari/src && sudo git pull
sudo /usr/local/go/bin/go build -trimpath -o /usr/local/bin/ari ./cmd/ari
sudo systemctl restart ari

# Docker
git pull && docker compose up -d --build
```

---

## Smoke test

Publish a test stream and confirm it goes live:

```bash
# RTMP
ffmpeg -re -f lavfi -i testsrc=size=640x480:rate=30 -c:v libx264 -preset ultrafast \
  -f flv rtmp://<host>:1935/live/demo

# SRT
ffmpeg -re -f lavfi -i testsrc=size=640x480:rate=30 -c:v libx264 -preset ultrafast \
  -f mpegts "srt://<host>:8890?streamid=demo&mode=caller"
```

```bash
curl http://<host>:8080/api/v1/streams
```

Then watch it at `http://<host>:8080/player.html?id=demo`.
