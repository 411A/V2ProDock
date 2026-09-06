FROM alpine:3.20

# IKEv2 + L2TP/IPsec sidecar: strongSwan + xl2tpd + TUN->SOCKS so VPN
# clients egress ONLY via the working Xray configs (never direct).
# No secrets baked in - certs/users are generated from env at runtime.
# NOTE: the hev probe below expects exit 255 (hev prints usage+Version and
# exits 255 without a config file). Any other nonzero rc fails the build.
RUN apk --no-cache add \
      strongswan \
      xl2tpd \
      ppp \
      iptables \
      iproute2 \
      openssl \
      curl \
      jq \
      ca-certificates \
      busybox-extras \
    && command -v xl2tpd && command -v pppd && command -v swanctl && command -v ipsec \
    && mkdir -p /etc/swanctl/conf.d /etc/swanctl/x509 /etc/swanctl/x509ca \
       /etc/swanctl/private /var/log /config/vpn \
    && ARCH=$(uname -m) && \
       case "$ARCH" in \
         x86_64)  HEV_ARCH="linux-x86_64" ;; \
         aarch64) HEV_ARCH="linux-arm64" ;; \
         armv7l)  HEV_ARCH="linux-arm32v7" ;; \
         *) echo "unsupported arch: $ARCH" >&2; exit 1 ;; \
       esac && \
       HEV_VER="2.17.1" && \
       curl -fSL "https://github.com/heiher/hev-socks5-tunnel/releases/download/${HEV_VER}/hev-socks5-tunnel-${HEV_ARCH}" \
         -o /usr/local/bin/hev-socks5-tunnel && \
       chmod +x /usr/local/bin/hev-socks5-tunnel && \
       test -s /usr/local/bin/hev-socks5-tunnel && \
       /usr/local/bin/hev-socks5-tunnel > /tmp/hev-probe.log 2>&1 || test $? -eq 255 && \
       grep -q "Version:" /tmp/hev-probe.log && \
       rm -f /tmp/hev-probe.log

COPY entrypoint.sh /entrypoint.sh
COPY watchdog.sh /watchdog.sh
COPY gen-mobileconfig.sh /gen-mobileconfig.sh
COPY swanctl.conf.tmpl /swanctl.conf.tmpl
COPY xl2tpd.conf.tmpl /xl2tpd.conf.tmpl
COPY options.xl2tpd.tmpl /options.xl2tpd.tmpl
RUN chmod +x /entrypoint.sh /watchdog.sh /gen-mobileconfig.sh

EXPOSE 500/udp 4500/udp 1701/udp

HEALTHCHECK --interval=30s --timeout=10s --retries=3 \
  CMD sh -c 'ipsec status >/dev/null 2>&1 || swanctl --list-sas >/dev/null 2>&1'

ENTRYPOINT ["/entrypoint.sh"]
