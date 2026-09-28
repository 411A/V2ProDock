FROM golang:1.25-alpine AS builder
ENV GOTOOLCHAIN=local
ENV GOPROXY=https://goproxy.cn,direct
WORKDIR /app

COPY v2proxy/go.mod v2proxy/go.sum ./
RUN go mod download

COPY v2proxy/ .

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o v2proxy .

FROM alpine:3.20
RUN apk --no-cache add ca-certificates curl unzip && \
    ARCH=$(uname -m) && \
    case "$ARCH" in \
      x86_64)  XARCH="64" ;; \
      aarch64) XARCH="arm64-v8a" ;; \
      armv7l)  XARCH="arm32-v7a" ;; \
      *)       XARCH="64" ;; \
    esac && \
    curl -sSL --retry 3 --retry-delay 2 --max-time 120 "https://github.com/XTLS/Xray-core/releases/latest/download/Xray-linux-${XARCH}.zip" -o /tmp/x.zip && \
    unzip -o /tmp/x.zip -d /root/xray && rm /tmp/x.zip && chmod +x /root/xray/xray && \
    /root/xray/xray version >/dev/null && \
    apk del unzip && \
    rm -rf /var/cache/apk/*

WORKDIR /root/
COPY --from=builder /app/v2proxy .
RUN mkdir -p /root/config

EXPOSE 27018-27100

# Same real-liveness rationale as compose: kill -0 passes on a wedged
# process. Shell form already runs under /bin/sh -c, so the runtime
# API_PORT default resolves without a nested shell.
HEALTHCHECK --interval=30s --timeout=10s --retries=3 --start-period=180s \
  CMD curl -fsS --max-time 5 http://127.0.0.1:${API_PORT:-27018}/health >/dev/null || exit 1

ENTRYPOINT ["./v2proxy"]
