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
    curl -sL "https://github.com/XTLS/Xray-core/releases/latest/download/Xray-linux-${XARCH}.zip" -o /tmp/x.zip && \
    unzip -o /tmp/x.zip -d /root/xray && rm /tmp/x.zip && chmod +x /root/xray/xray && \
    apk del unzip && \
    rm -rf /var/cache/apk/*

WORKDIR /root/
COPY --from=builder /app/v2proxy .
RUN mkdir -p /root/config

EXPOSE 27018-27100

HEALTHCHECK --interval=30s --timeout=10s --retries=3 \
  CMD kill -0 1 2>/dev/null || exit 1

ENTRYPOINT ["./v2proxy"]
