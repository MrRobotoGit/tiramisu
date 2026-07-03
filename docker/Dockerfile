FROM golang:1.24-bookworm AS build

RUN apt-get update \
    && apt-get install -y --no-install-recommends build-essential pkg-config libfuse3-dev ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY . .

# Use Docker's automatic TARGETARCH variable for multi-arch support
ARG TARGETARCH
ARG APP_VERSION=dev
# CGO_ENABLED=1 is required for FUSE integration.
# -pgo=auto ensures we use profile-guided optimization if default.pgo is present.
RUN CGO_ENABLED=1 GOOS=linux GOARCH=$TARGETARCH go build -pgo=auto -ldflags "-X main.AppVersion=${APP_VERSION}" -o /out/tiramisu .

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
      ca-certificates \
      fuse3 \
      iptables \
      tini \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=build /out/tiramisu /usr/local/bin/tiramisu
COPY docker/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh

RUN chmod +x /usr/local/bin/docker-entrypoint.sh

ENV MKV_PROXY_CONFIG_PATH=/config.json
ENV TIRAMISU_ROOT_PATH=/usr/local
ENV TIRAMISU_SOURCE_PATH=/mnt/tiramisu-mkv-real
ENV TIRAMISU_MOUNT_PATH=/mnt/tiramisu-mkv-virtual
ENV TIRAMISU_STATE_DIR=/usr/local/STATE
ENV TIRAMISU_LOG_DIR=/usr/local/logs
# Deprecated aliases, still honored by docker-entrypoint.sh for existing users
ENV GOSTREAM_ROOT_PATH=/usr/local
ENV GOSTREAM_SOURCE_PATH=/mnt/tiramisu-mkv-real
ENV GOSTREAM_MOUNT_PATH=/mnt/tiramisu-mkv-virtual
ENV GOSTREAM_STATE_DIR=/usr/local/STATE
ENV GOSTREAM_LOG_DIR=/usr/local/logs

EXPOSE 8090 9080

# tini is used as PID 1 to forward signals (e.g. SIGTERM on docker stop) to
# tiramisu and reap zombie processes. Without it, the shell entrypoint script
# would be PID 1 and would not forward signals, causing an unclean shutdown.
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/docker-entrypoint.sh"]
