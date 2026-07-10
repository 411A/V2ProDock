FROM golang:1.23-alpine AS builder
WORKDIR /app
COPY v2proxy/ .
RUN GOTOOLCHAIN=auto go mod tidy && GOTOOLCHAIN=auto CGO_ENABLED=0 GOOS=linux go build -o v2proxy .

FROM alpine:3.20
RUN apk --no-cache add ca-certificates curl unzip wget && \
    ARCH=$(uname -m) && \
    case "$ARCH" in \
      x86_64)  XARCH="64" ;; \
      aarch64) XARCH="arm64-v8a" ;; \
      armv7l)  XARCH="arm32-v7a" ;; \
      *)       XARCH="64" ;; \
    esac && \
    curl -sL "https://github.com/XTLS/Xray-core/releases/latest/download/Xray-linux-${XARCH}.zip" -o /tmp/x.zip && \
    unzip -o /tmp/x.zip -d /root/xray && rm /tmp/x.zip && chmod +x /root/xray/xray && \
    apk del unzip

WORKDIR /root/
COPY --from=builder /app/v2proxy .
RUN mkdir -p /root/config

EXPOSE 27019 27020

HEALTHCHECK --interval=30s --timeout=10s --retries=3 \
  CMD wget -q --spider http://127.0.0.1:27020 || exit 1

ENTRYPOINT ["./v2proxy"]
