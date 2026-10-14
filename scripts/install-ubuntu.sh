#!/usr/bin/env bash
#
# Ari Media Engine - one-command installer for Ubuntu 24.04.
#
#   sudo ./scripts/install-ubuntu.sh
#
# It installs dependencies (ffmpeg, Go toolchain), builds the engine, creates a
# dedicated service user, writes /etc/ari/{config.yaml,ari.env}, installs and
# starts a systemd unit, and opens the firewall ports when ufw is active.
#
# All inputs are optional environment variables:
#   ARI_DOMAIN         Public hostname for HTTPS embeds (e.g. media.example.com)
#   ARI_PUBLIC_URL     Full public base URL (default http://<public-ip>:<port>)
#   ARI_PUBLIC_IP      Public IP advertised for WebRTC ICE (auto-detected)
#   ARI_HTTP_PORT      HTTP/API port (default 80)
#   ARI_RTMP_PORT      RTMP ingest port (default 1935)
#   ARI_SRT_PORT       SRT ingest port (default 8890)
#   ARI_ICE_TCP_PORT   ICE-TCP port (default 7881)
#   ARI_ICE_UDP_MIN    ICE UDP range start (default 50000)
#   ARI_ICE_UDP_MAX    ICE UDP range end (default 50100)
#   ARI_TLS_CERT       TLS certificate path (enables HTTPS)
#   ARI_TLS_KEY        TLS private key path
#   ARI_ENABLE_AUTH    1 to require dashboard login (default 1)
#   ARI_ADMIN_USER     dashboard username (default admin)
#   ARI_ADMIN_PASSWORD dashboard password (prompted; auto-generated if blank)
#   ARI_ENABLE_TOKEN   1 to tokenize HLS/DASH (default 0)
#   ARI_ENABLE_CLUSTER 1 to enable Redis clustering (default 0)
#   ARI_REPO_URL       Git URL (default the GitHub repo)
#   ARI_REF            Git ref/branch to build (default main)
#   ARI_INSTALL_DIR    Install root (default /opt/ari)
#
set -euo pipefail

ARI_REPO_URL="${ARI_REPO_URL:-https://github.com/johndesu090/ari-media-engine.git}"
ARI_REF="${ARI_REF:-main}"
ARI_INSTALL_DIR="${ARI_INSTALL_DIR:-/opt/ari}"
ARI_HTTP_PORT="${ARI_HTTP_PORT:-80}"
ARI_RTMP_PORT="${ARI_RTMP_PORT:-1935}"
ARI_SRT_PORT="${ARI_SRT_PORT:-8890}"
ARI_ICE_TCP_PORT="${ARI_ICE_TCP_PORT:-7881}"
ARI_ICE_UDP_MIN="${ARI_ICE_UDP_MIN:-50000}"
ARI_ICE_UDP_MAX="${ARI_ICE_UDP_MAX:-50100}"
ARI_TLS_CERT="${ARI_TLS_CERT:-}"
ARI_TLS_KEY="${ARI_TLS_KEY:-}"
ARI_ENABLE_AUTH="${ARI_ENABLE_AUTH:-1}"
ARI_ENABLE_TOKEN="${ARI_ENABLE_TOKEN:-0}"
ARI_ENABLE_CLUSTER="${ARI_ENABLE_CLUSTER:-0}"
ARI_ADMIN_USER="${ARI_ADMIN_USER:-admin}"
ARI_ADMIN_PASSWORD="${ARI_ADMIN_PASSWORD:-}"
ARI_GO_VERSION="${ARI_GO_VERSION:-1.26.0}"

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[warn]\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31m[error]\033[0m %s\n' "$*" >&2; exit 1; }

require_root() {
  [ "$(id -u)" -eq 0 ] || die "run this installer as root: sudo $0"
}

detect_public_ip() {
  [ -n "${ARI_PUBLIC_IP:-}" ] && { echo "$ARI_PUBLIC_IP"; return; }
  local ip
  ip="$(curl -fsS --max-time 5 https://api.ipify.org 2>/dev/null || true)"
  [ -z "$ip" ] && ip="$(curl -fsS --max-time 5 https://ifconfig.me 2>/dev/null || true)"
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

install_packages() {
  log "Installing system packages"
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -y
  apt-get install -y --no-install-recommends ffmpeg curl ca-certificates git
}

install_go() {
  if command -v go >/dev/null 2>&1; then
    local cur
    cur="$(go env GOVERSION 2>/dev/null | sed 's/^go//')"
    if [ -n "$cur" ] && [ "$(printf '%s\n%s\n' "1.26.0" "$cur" | sort -V | head -n1)" = "1.26.0" ]; then
      log "Go $cur already installed"
      return
    fi
  fi
  log "Installing Go ${ARI_GO_VERSION}"
  local arch tarball
  case "$(uname -m)" in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) die "unsupported architecture: $(uname -m)" ;;
  esac
  tarball="go${ARI_GO_VERSION}.linux-${arch}.tar.gz"
  curl -fsSL -o "/tmp/${tarball}" "https://go.dev/dl/${tarball}"
  rm -rf /usr/local/go
  tar -C /usr/local -xzf "/tmp/${tarball}"
  rm -f "/tmp/${tarball}"
  ln -sf /usr/local/go/bin/go /usr/local/bin/go
  ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt
}

fetch_and_build() {
  local src="${ARI_INSTALL_DIR}/src"
  log "Fetching source into ${src}"
  if [ -d "${src}/.git" ]; then
    git -C "${src}" fetch --all --prune
    git -C "${src}" checkout "${ARI_REF}"
    git -C "${src}" pull --ff-only || true
  else
    mkdir -p "${ARI_INSTALL_DIR}"
    git clone --branch "${ARI_REF}" "${ARI_REPO_URL}" "${src}"
  fi

  log "Building the engine"
  ( cd "${src}" && go build -trimpath -o /usr/local/bin/ari ./cmd/ari )

  log "Installing web assets"
  mkdir -p "${ARI_INSTALL_DIR}/web"
  rm -rf "${ARI_INSTALL_DIR}/web/dashboard" "${ARI_INSTALL_DIR}/web/p2p"
  cp -r "${src}/web/dashboard" "${ARI_INSTALL_DIR}/web/dashboard"
  cp -r "${src}/web/p2p" "${ARI_INSTALL_DIR}/web/p2p"
}

create_user_and_dirs() {
  log "Creating service user and directories"
  id -u ari >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin ari
  mkdir -p /etc/ari /var/lib/ari/hls /var/lib/ari/recordings
  chown -R ari:ari /var/lib/ari
  chown -R ari:ari "${ARI_INSTALL_DIR}/web"
}

# prompt_admin_password asks for the dashboard password when auth is enabled and
# no password was supplied. Piped runs (no TTY) fall back to auto-generation.
prompt_admin_password() {
  [ "${ARI_ENABLE_AUTH}" = "1" ] || return 0
  [ -n "${ARI_ADMIN_PASSWORD}" ] && return 0
  [ -t 0 ] || return 0

  local p1 p2
  while :; do
    printf '\033[1;34m==>\033[0m Dashboard password for "%s" (leave blank to auto-generate): ' "${ARI_ADMIN_USER}"
    read -rs p1 || true
    printf '\n'
    [ -z "$p1" ] && return 0
    printf '    Confirm password: '
    read -rs p2 || true
    printf '\n'
    if [ "$p1" = "$p2" ]; then
      ARI_ADMIN_PASSWORD="$p1"
      return 0
    fi
    warn "passwords do not match, try again"
  done
}

write_config() {
  local public_ip public_url api_key token_secret admin_password
  public_ip="$(detect_public_ip)"
  public_url="${ARI_PUBLIC_URL:-http://${public_ip}:${ARI_HTTP_PORT}}"
  if [ -n "${ARI_DOMAIN:-}" ]; then
    public_url="https://${ARI_DOMAIN}"
  fi
  api_key="$(rand_hex)"
  token_secret="$(rand_hex)"
  admin_password="${ARI_ADMIN_PASSWORD:-$(rand_hex)}"

  log "Writing /etc/ari/config.yaml"
  cp "${ARI_INSTALL_DIR}/src/configs/config.yaml" /etc/ari/config.yaml

  log "Writing /etc/ari/ari.env"
  cat > /etc/ari/ari.env <<EOF
# Generated by install-ubuntu.sh - environment overrides for Ari Media Engine.
ARI_SERVER_PUBLIC_URL=${public_url}
ARI_SERVER_PORT=${ARI_HTTP_PORT}
ARI_ICE_PUBLIC_IPS=${public_ip}
ARI_ICE_TCP_PORT=${ARI_ICE_TCP_PORT}
ARI_ICE_UDP_PORT_MIN=${ARI_ICE_UDP_MIN}
ARI_ICE_UDP_PORT_MAX=${ARI_ICE_UDP_MAX}
ARI_WEBRTC_ENABLE_LOOPBACK=false
ARI_RTMP_ENABLED=true
ARI_RTMP_PORT=${ARI_RTMP_PORT}
ARI_SRT_ENABLED=true
ARI_SRT_PORT=${ARI_SRT_PORT}
ARI_HLS_ENABLED=true
ARI_HLS_OUTPUT_DIR=/var/lib/ari/hls
ARI_STORAGE_DRIVER=local
ARI_STORAGE_LOCAL_ROOT=/var/lib/ari/recordings
ARI_RECORDING_WORK_DIR=/var/lib/ari/recordings
ARI_TRANSCODE_OUTPUT_DIR=/var/lib/ari/hls
ARI_AUTH_ENABLED=${ARI_ENABLE_AUTH}
ARI_AUTH_USERNAME=${ARI_ADMIN_USER}
ARI_AUTH_PASSWORD=${admin_password}
ARI_API_KEY=${api_key}
ARI_TOKEN_ENABLED=${ARI_ENABLE_TOKEN}
ARI_TOKEN_SECRET=${token_secret}
ARI_CLUSTER_ENABLED=${ARI_ENABLE_CLUSTER}
ARI_REDIS_URL=redis://127.0.0.1:6379/0
EOF
  chmod 600 /etc/ari/ari.env

  if [ -n "${ARI_TLS_CERT}" ] && [ -n "${ARI_TLS_KEY}" ]; then
    {
      echo "ARI_TLS_CERT_FILE=${ARI_TLS_CERT}"
      echo "ARI_TLS_KEY_FILE=${ARI_TLS_KEY}"
    } >> /etc/ari/ari.env
  fi

  echo
  log "Configuration summary"
  echo "    public URL   : ${public_url}"
  echo "    ICE public IP: ${public_ip}"
  if [ "${ARI_ENABLE_AUTH}" = "1" ]; then
    echo "    dashboard    : ${ARI_ADMIN_USER} / ${admin_password}"
  else
    echo "    dashboard    : authentication disabled"
  fi
  echo "    API key      : ${api_key}"
  echo "    token secret : ${token_secret}$([ "${ARI_ENABLE_TOKEN}" = "1" ] || echo '  (tokenization disabled)')"

  RESOLVED_PUBLIC_URL="${public_url}"
}

install_service() {
  log "Installing systemd unit"
  cat > /etc/systemd/system/ari.service <<EOF
[Unit]
Description=Ari Media Engine
After=network-online.target
Wants=network-online.target

[Service]
User=ari
Group=ari
WorkingDirectory=${ARI_INSTALL_DIR}
EnvironmentFile=-/etc/ari/ari.env
ExecStart=/usr/local/bin/ari -config /etc/ari/config.yaml
Restart=on-failure
RestartSec=3
LimitNOFILE=65536
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable ari >/dev/null 2>&1 || true
  systemctl restart ari
}

open_firewall() {
  if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q "Status: active"; then
    log "Opening firewall ports (ufw)"
    ufw allow "${ARI_HTTP_PORT}/tcp"          >/dev/null 2>&1 || true
    ufw allow 443/tcp                         >/dev/null 2>&1 || true
    ufw allow "${ARI_RTMP_PORT}/tcp"          >/dev/null 2>&1 || true
    ufw allow "${ARI_SRT_PORT}/udp"           >/dev/null 2>&1 || true
    ufw allow "${ARI_SRT_PORT}/tcp"           >/dev/null 2>&1 || true
    ufw allow "${ARI_ICE_TCP_PORT}/tcp"       >/dev/null 2>&1 || true
    ufw allow "${ARI_ICE_UDP_MIN}:${ARI_ICE_UDP_MAX}/udp" >/dev/null 2>&1 || true
  else
    warn "ufw is not active; open ports ${ARI_HTTP_PORT}/tcp, 443/tcp, ${ARI_RTMP_PORT}/tcp, ${ARI_SRT_PORT}/udp+tcp, ${ARI_ICE_TCP_PORT}/tcp, ${ARI_ICE_UDP_MIN}-${ARI_ICE_UDP_MAX}/udp manually"
  fi
}

verify() {
  log "Waiting for the engine to come up"
  local scheme="http" curl_flags=()
  if [ -n "${ARI_TLS_CERT}" ] && [ -n "${ARI_TLS_KEY}" ]; then
    scheme="https"
    curl_flags=(-k)
  fi
  for _ in $(seq 1 20); do
    if curl -fsS "${curl_flags[@]}" --max-time 2 "${scheme}://127.0.0.1:${ARI_HTTP_PORT}/healthz" >/dev/null 2>&1; then
      log "Ari Media Engine is running"
      echo "    Dashboard: ${RESOLVED_PUBLIC_URL:-${scheme}://127.0.0.1:${ARI_HTTP_PORT}}/"
      echo "    Health   : ${scheme}://127.0.0.1:${ARI_HTTP_PORT}/healthz"
      echo "    Logs     : journalctl -u ari -f"
      if [ -n "${ARI_DOMAIN:-}" ]; then
        warn "Cloudflare: set SSL/TLS to Flexible for an HTTP origin, or Full (strict) with an Origin Certificate and TLS enabled here - see docs/deployment.md"
      fi
      return 0
    fi
    sleep 1
  done
  warn "the service did not answer /healthz yet; check: journalctl -u ari -e"
}

main() {
  require_root
  if [ -r /etc/os-release ]; then
    # shellcheck disable=SC1091
    . /etc/os-release
    [ "${ID:-}" = "ubuntu" ] || warn "this installer targets Ubuntu 24.04 (detected ${ID:-unknown})"
  fi
  install_packages
  install_go
  fetch_and_build
  create_user_and_dirs
  prompt_admin_password
  write_config
  install_service
  open_firewall
  verify
}

main "$@"
