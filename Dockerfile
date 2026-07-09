FROM golang:1.23-alpine AS builder
WORKDIR /app
COPY v2proxy/ .
RUN go mod tidy && CGO_ENABLED=0 GOOS=linux go build -o v2proxy .

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
    unzip /tmp/x.zip -d /root/xray && rm /tmp/x.zip && chmod +x /root/xray/xray && \
    apk del curl unzip

WORKDIR /root/
COPY --from=builder /app/v2proxy .
RUN mkdir -p /root/config

EXPOSE 1080 27019
ENTRYPOINT ["./v2proxy"]
