FROM alpine:3.20

# IKEv2 + L2TP/IPsec + PPTP sidecar: strongSwan + xl2tpd + pptpd + TUN->SOCKS
# so VPN clients egress ONLY via the working Xray configs (never direct).
# No secrets baked in - certs/users are generated from env at runtime.
# pptpd (poptop 1.4.0) is NOT in Alpine repos, so it is compiled from source
# (only the pptpd + pptpctrl binaries; bcrelay is skipped - it does not build
# on musl and is not needed for a VPN server). PPTP is OFF by default
# (VPN_ENABLE_PPTP=0): it exists only for ancient LAN devices (e.g. satellite
# receivers) that speak nothing else. MPPE-128 is always required when on.
# NOTE: the hev probe below expects exit 255 (hev prints usage+Version and
# exits 255 without a config file). Any other nonzero rc fails the build.
RUN apk --no-cache add \
      strongswan \
      xl2tpd \
      ppp \
      iptables \
      iproute2 \
      conntrack-tools \
      openssl \
      curl \
      jq \
      ca-certificates \
      busybox-extras \
    && command -v xl2tpd && command -v pppd && command -v swanctl && command -v ipsec \
    && mkdir -p /etc/swanctl/conf.d /etc/swanctl/x509 /etc/swanctl/x509ca \
       /etc/swanctl/private /var/log /config/vpn /etc/pptpd /etc/ppp \
    && apk add --no-cache --virtual .pptpd-build build-base bsd-compat-headers wget \
    && cd /tmp \
    && wget -q "https://sourceforge.net/projects/poptop/files/pptpd/pptpd-1.4.0/pptpd-1.4.0.tar.gz/download" -O pptpd.tar.gz \
    && tar -xzf pptpd.tar.gz \
    && cd pptpd-1.4.0 \
    && ./configure \
    && make pptpd pptpctrl \
    && mkdir -p /usr/local/sbin \
    && install -m755 pptpd /usr/local/sbin/pptpd \
    && install -m755 pptpctrl /usr/local/sbin/pptpctrl \
    && /usr/local/sbin/pptpd --help 2>&1 | grep -q "pptpd" \
    && cd / && rm -rf /tmp/pptpd-1.4.0 /tmp/pptpd.tar.gz \
    && apk del .pptpd-build \
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
COPY pptpd.conf.tmpl /pptpd.conf.tmpl
COPY options.pptpd.tmpl /options.pptpd.tmpl
COPY pptp-ip-up.sh /etc/ppp/pptp-ip-up
COPY pptp-ip-down.sh /etc/ppp/pptp-ip-down
RUN chmod +x /entrypoint.sh /watchdog.sh /gen-mobileconfig.sh \
      /etc/ppp/pptp-ip-up /etc/ppp/pptp-ip-down \
    && command -v conntrack && conntrack --version

EXPOSE 500/udp 4500/udp 1701/udp 1723/tcp

HEALTHCHECK --interval=30s --timeout=10s --retries=3 \
  CMD sh -c 'ipsec status >/dev/null 2>&1 || swanctl --list-sas >/dev/null 2>&1'

ENTRYPOINT ["/entrypoint.sh"]
