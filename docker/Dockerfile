FROM golang:1.24-bookworm AS build

RUN apt-get update \
    && apt-get install -y --no-install-recommends build-essential pkg-config libfuse3-dev ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY . .

RUN CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -o /out/gostream .

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
      ca-certificates \
      fuse3 \
      iptables \
      tini \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=build /out/gostream /usr/local/bin/gostream
COPY docker/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh

RUN chmod +x /usr/local/bin/docker-entrypoint.sh

ENV MKV_PROXY_CONFIG_PATH=/config.json
ENV GOSTREAM_SOURCE_PATH=/mnt/gostream-mkv-real
ENV GOSTREAM_MOUNT_PATH=/mnt/gostream-mkv-virtual

# tini is used as PID 1 to forward signals (e.g. SIGTERM on docker stop) to
# gostream and reap zombie processes. Without it, the shell entrypoint script
# would be PID 1 and would not forward signals, causing an unclean shutdown.
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/docker-entrypoint.sh"]
