# syntax=docker/dockerfile:1.7
# ---------- 1. build the web UI ----------
FROM --platform=$BUILDPLATFORM node:22-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json* ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

# ---------- 2. build the Go binary ----------
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
COPY third_party/pocket-core ./third_party/pocket-core
RUN cd third_party/pocket-core && sha256sum -c SHA256SUMS
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/pocket-monitoring ./cmd/pocket-monitoring

# ---------- 3. the Ookla Speedtest CLI (Internet tab), checksum-pinned ----------
FROM --platform=$BUILDPLATFORM alpine:3.22 AS ookla
ARG TARGETARCH TARGETVARIANT
ARG OOKLA_VERSION=1.2.0
RUN set -eu; \
    case "${TARGETARCH}${TARGETVARIANT}" in \
      amd64)  a=x86_64;  sum=5690596c54ff9bed63fa3732f818a05dbc2db19ad36ed68f21ca5f64d5cfeeb7 ;; \
      arm64)  a=aarch64; sum=3953d231da3783e2bf8904b6dd72767c5c6e533e163d3742fd0437affa431bd3 ;; \
      armv7)  a=armhf;   sum=e45fcdebbd8a185553535533dd032d6b10bc8c64eee4139b1147b9c09835d08d ;; \
      *) echo "no Speedtest CLI for ${TARGETARCH}${TARGETVARIANT}"; mkdir -p /out; exit 0 ;; \
    esac; \
    wget -qO /tmp/ookla.tgz "https://install.speedtest.net/app/cli/ookla-speedtest-${OOKLA_VERSION}-linux-${a}.tgz"; \
    echo "${sum}  /tmp/ookla.tgz" | sha256sum -c -; \
    mkdir -p /out && tar -xzf /tmp/ookla.tgz -C /out speedtest

# ---------- 4. runtime ----------
FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata curl \
 && addgroup -g 1000 app && adduser -u 1000 -G app -D -h /data app
COPY --from=build /out/pocket-monitoring /usr/local/bin/pocket-monitoring
# Ookla's CLI: free for personal, non-commercial use (https://www.speedtest.net/about/eula);
# Pocket Monitoring only runs it once someone turns speed tests on.
COPY --from=ookla /out/ /usr/local/bin/
ENV DATA_DIR=/data PORT=8080 HOST=0.0.0.0
VOLUME ["/data"]
EXPOSE 8080 8193
USER app
WORKDIR /data
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 CMD curl -fsS http://localhost:8080/healthz || exit 1
LABEL org.opencontainers.image.title="Pocket Monitoring" \
      org.opencontainers.image.description="Server monitoring with a memory: CPU, memory, disks, temperatures, processes, containers and internet speed of this box and any other, live and as history, with alerts. Also the agent for other machines (pocket-monitoring agent)." \
      org.opencontainers.image.source="https://github.com/abjelosevic88/pocket-homelab-monitoring" \
      org.opencontainers.image.licenses="MIT"
ENTRYPOINT ["pocket-monitoring"]
