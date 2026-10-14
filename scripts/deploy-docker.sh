#!/usr/bin/env bash
#
# Ari Media Engine - one-command Docker deployment.
#
#   ./scripts/deploy-docker.sh
#
# Builds the image and (re)starts a container. Host networking is used by default
# so WebRTC UDP works without a large port map.
#
# Optional environment variables:
#   ARI_PUBLIC_URL     Public base URL (default http://<public-ip>:<port>)
#   ARI_PUBLIC_IP      Public IP for WebRTC ICE (auto-detected)
#   ARI_HTTP_PORT      HTTP/API port (default 8080)
#   ARI_ICE_UDP_MIN    ICE UDP range start (default 50000)
#   ARI_ICE_UDP_MAX    ICE UDP range end (default 50100)
#   ARI_ENABLE_AUTH    1 to require an API key (default 0)
#   ARI_ENABLE_TOKEN   1 to tokenize HLS/DASH (default 0)
#   ARI_IMAGE          Image tag (default ari-media-engine:latest)
#   ARI_CONTAINER      Container name (default ari)
#   ARI_NETWORK        host | bridge (default host)
#   ARI_DATA_DIR       Host data directory (default ./data)
#   ARI_SKIP_BUILD     1 to reuse the existing image (default 0)
#
set -euo pipefail

ARI_IMAGE="${ARI_IMAGE:-ari-media-engine:latest}"
ARI_CONTAINER="${ARI_CONTAINER:-ari}"
ARI_NETWORK="${ARI_NETWORK:-host}"
ARI_HTTP_PORT="${ARI_HTTP_PORT:-8080}"
ARI_RTMP_PORT="${ARI_RTMP_PORT:-1935}"
ARI_SRT_PORT="${ARI_SRT_PORT:-8890}"
ARI_ICE_TCP_PORT="${ARI_ICE_TCP_PORT:-7881}"
ARI_ICE_UDP_MIN="${ARI_ICE_UDP_MIN:-50000}"
ARI_ICE_UDP_MAX="${ARI_ICE_UDP_MAX:-50100}"
ARI_ENABLE_AUTH="${ARI_ENABLE_AUTH:-0}"
ARI_ENABLE_TOKEN="${ARI_ENABLE_TOKEN:-0}"
ARI_DATA_DIR="${ARI_DATA_DIR:-$(pwd)/data}"
ARI_SKIP_BUILD="${ARI_SKIP_BUILD:-0}"

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31m[error]\033[0m %s\n' "$*" >&2; exit 1; }

command -v docker >/dev/null 2>&1 || die "docker is not installed"
docker info >/dev/null 2>&1 || die "the docker daemon is not running"

detect_public_ip() {
  [ -n "${ARI_PUBLIC_IP:-}" ] && { echo "$ARI_PUBLIC_IP"; return; }
  local ip
  ip="$(curl -fsS --max-time 5 https://api.ipify.org 2>/dev/null || true)"
  [ -z "$ip" ] && ip="127.0.0.1"
  echo "$ip"
}

rand_hex() {
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex 16
  else
    head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n'
  fi
}

PUBLIC_IP="$(detect_public_ip)"
PUBLIC_URL="${ARI_PUBLIC_URL:-http://${PUBLIC_IP}:${ARI_HTTP_PORT}}"
API_KEY="$(rand_hex)"
TOKEN_SECRET="$(rand_hex)"

if [ "${ARI_SKIP_BUILD}" != "1" ]; then
  log "Building image ${ARI_IMAGE}"
  docker build -t "${ARI_IMAGE}" .
fi

log "Recreating container ${ARI_CONTAINER}"
docker rm -f "${ARI_CONTAINER}" >/dev/null 2>&1 || true

mkdir -p "${ARI_DATA_DIR}/recordings" "${ARI_DATA_DIR}/hls"

ENV_ARGS=(
  -e "ARI_SERVER_PUBLIC_URL=${PUBLIC_URL}"
  -e "ARI_ICE_PUBLIC_IPS=${PUBLIC_IP}"
  -e "ARI_ICE_TCP_PORT=${ARI_ICE_TCP_PORT}"
  -e "ARI_ICE_UDP_PORT_MIN=${ARI_ICE_UDP_MIN}"
  -e "ARI_ICE_UDP_PORT_MAX=${ARI_ICE_UDP_MAX}"
  -e "ARI_AUTH_ENABLED=${ARI_ENABLE_AUTH}"
  -e "ARI_API_KEY=${API_KEY}"
  -e "ARI_TOKEN_ENABLED=${ARI_ENABLE_TOKEN}"
  -e "ARI_TOKEN_SECRET=${TOKEN_SECRET}"
  -e "ARI_STORAGE_LOCAL_ROOT=/app/data/recordings"
  -e "ARI_HLS_OUTPUT_DIR=/app/data/hls"
  -e "ARI_RECORDING_WORK_DIR=/app/data/recordings"
  -e "ARI_TRANSCODE_OUTPUT_DIR=/app/data/hls"
)

if [ "${ARI_NETWORK}" = "host" ]; then
  NET_ARGS=(--network host)
else
  NET_ARGS=(
    -p "${ARI_HTTP_PORT}:8080"
    -p "${ARI_RTMP_PORT}:1935"
    -p "${ARI_SRT_PORT}:8890/tcp"
    -p "${ARI_SRT_PORT}:8890/udp"
    -p "${ARI_ICE_TCP_PORT}:7881/tcp"
    -p "${ARI_ICE_UDP_MIN}-${ARI_ICE_UDP_MAX}:${ARI_ICE_UDP_MIN}-${ARI_ICE_UDP_MAX}/udp"
  )
fi

docker run -d --name "${ARI_CONTAINER}" --restart unless-stopped \
  "${NET_ARGS[@]}" "${ENV_ARGS[@]}" \
  -v "${ARI_DATA_DIR}:/app/data" \
  "${ARI_IMAGE}" >/dev/null

log "Waiting for the engine to come up"
for _ in $(seq 1 20); do
  if curl -fsS --max-time 2 "http://127.0.0.1:${ARI_HTTP_PORT}/healthz" >/dev/null 2>&1; then
    log "Ari Media Engine is running"
    echo "    Dashboard: ${PUBLIC_URL}/"
    echo "    Logs     : docker logs -f ${ARI_CONTAINER}"
    echo "    API key  : ${API_KEY}$([ "${ARI_ENABLE_AUTH}" = "1" ] || echo '  (auth disabled)')"
    exit 0
  fi
  sleep 1
done

printf '\033[1;33m[warn]\033[0m the container did not answer /healthz; check: docker logs %s\n' "${ARI_CONTAINER}"
