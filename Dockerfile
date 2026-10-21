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

# ---------- 3. runtime ----------
FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata curl \
 && addgroup -g 1000 app && adduser -u 1000 -G app -D -h /data app
COPY --from=build /out/pocket-monitoring /usr/local/bin/pocket-monitoring
ENV DATA_DIR=/data PORT=8080 HOST=0.0.0.0
VOLUME ["/data"]
EXPOSE 8080 8193
USER app
WORKDIR /data
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 CMD curl -fsS http://localhost:8080/healthz || exit 1
LABEL org.opencontainers.image.title="Pocket Monitoring" \
      org.opencontainers.image.description="Server monitoring with a memory: CPU, memory, disks, temperatures, processes and containers of this box and any other, live and as history, with alerts. Also the agent for other machines (pocket-monitoring agent)." \
      org.opencontainers.image.source="https://github.com/abjelosevic88/pocket-homelab-monitoring" \
      org.opencontainers.image.licenses="MIT"
ENTRYPOINT ["pocket-monitoring"]
