# syntax=docker/dockerfile:1

# ---- build stage -----------------------------------------------------------
FROM golang:1.26-alpine AS build

WORKDIR /src

RUN apk add --no-cache git

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/ari ./cmd/ari

# ---- runtime stage ---------------------------------------------------------
FROM debian:bookworm-slim

# ffmpeg backs the ABR transcode pipeline; ca-certificates enable TLS to S3/Redis.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ffmpeg ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=build /out/ari /usr/local/bin/ari
COPY configs/config.yaml /app/config.yaml
COPY web /app/web

RUN mkdir -p /app/data/recordings /app/data/hls

# HTTP/API, RTMP, SRT, ICE-TCP.
EXPOSE 8080 1935 8890/tcp 8890/udp 7881/tcp
# ICE media UDP range (must match webrtc.ice_udp_port_min/max).
EXPOSE 50000-50100/udp

ENTRYPOINT ["/usr/local/bin/ari"]
CMD ["-config", "/app/config.yaml"]
