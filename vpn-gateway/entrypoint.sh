#!/bin/sh
# vpn-gateway entrypoint: IKEv2 + L2TP/IPsec gateway whose clients egress
# ONLY via working Xray configs (through tun0 -> SOCKS -> v2prodock).
# Strict: fails fast on bad env, never falls back to direct egress.
set -eu

log() { echo "[v2prodock-vpn] $*"; }
die() { echo "[v2prodock-vpn][FATAL] $*" >&2; exit 1; }

# VPN_ENABLED=0 lets existing proxy-only users keep `compose up` green.
case "${VPN_ENABLED:-1}" in
  0|false|False|FALSE|no|No|NO|off|Off|OFF)
    log "VPN_ENABLED=0: v2prodock-vpn idle (proxy-only mode). Set VPN_ENABLED=1 + VPN_PASSWORD to serve IKEv2."
    exec tail -f /dev/null
    ;;
esac

# ---- env defaults ----
VPN_DOMAIN="${VPN_DOMAIN:-vpn.local}"
VPN_SUBNET="${VPN_SUBNET:-10.10.10.0/24}"
VPN_L2TP_NET="${VPN_L2TP_NET:-10.10.11.0/24}"
VPN_DNS="${VPN_DNS:-1.1.1.1,8.8.8.8}"
VPN_USER="${VPN_USER:-vpnuser}"
VPN_PASSWORD="${VPN_PASSWORD:-}"
VPN_USERS="${VPN_USERS:-}"
VPN_EXTRA_SANS="${VPN_EXTRA_SANS:-}"
VPN_IPSEC_PSK="${VPN_IPSEC_PSK:-}"
VPN_L2TP_LOCAL="${VPN_L2TP_LOCAL:-10.10.11.1}"
VPN_L2TP_RANGE="${VPN_L2TP_RANGE:-10.10.11.10-10.10.11.100}"
# PPTP for ancient LAN devices only (e.g. satellite receivers that speak
# nothing else). OFF by default: PPTP/MPPE is cryptographically broken, so
# the PPTP leg must stay on a trusted LAN; internet egress still goes via
# the working Xray proxy. GRE (IP proto 47) needs host conntrack help
# (nf_conntrack_pptp) behind Docker NAT, or host networking on Linux.
VPN_ENABLE_PPTP="${VPN_ENABLE_PPTP:-0}"
VPN_PPTP_NET="${VPN_PPTP_NET:-10.10.12.0/24}"
VPN_PPTP_LOCAL="${VPN_PPTP_LOCAL:-10.10.12.1}"
VPN_PPTP_RANGE="${VPN_PPTP_RANGE:-10.10.12.10-10.10.12.100}"
V2PRODOCK_API="${V2PRODOCK_API:-http://v2prodock:27018/proxies}"
V2PRODOCK_SOCKS_HOST="${V2PRODOCK_SOCKS_HOST:-v2prodock}"
TUN_DEV="${TUN_DEV:-tun0}"
TUN_ADDR="${TUN_ADDR:-198.18.0.1}"
TUN_MTU="${TUN_MTU:-1500}"
PERSIST_DIR="${PERSIST_DIR:-/config/vpn}"
SWANCTL_CONF="${SWANCTL_CONF:-/etc/swanctl/swanctl.conf}"

mkdir -p "$PERSIST_DIR" /etc/swanctl/x509 /etc/swanctl/x509ca /etc/swanctl/private

# ---- credential validation (strict) ----
if [ -n "$VPN_USERS" ]; then
  # format user1:pass1,user2:pass2
  case "$VPN_USERS" in *:* ) ;; *) die "VPN_USERS must be user:pass[,user:pass...]" ;; esac
else
  [ -n "$VPN_PASSWORD" ] || die "Set VPN_PASSWORD (or VPN_USERS=user:pass,...). Refusing to boot with empty creds."
  [ "${#VPN_PASSWORD}" -ge 8 ] || die "VPN_PASSWORD must be >= 8 chars."
  case "$VPN_USER" in *:*|*" "*|"") die "VPN_USER must be non-empty without ':' or spaces." ;; esac
  VPN_USERS="${VPN_USER}:${VPN_PASSWORD}"
fi
# L2TP/IPsec needs its own pre-shared key (file-less login for legacy devices).
[ -n "$VPN_IPSEC_PSK" ] || die "Set VPN_IPSEC_PSK (IPsec pre-shared key for L2TP clients, min 8 chars)."
[ "${#VPN_IPSEC_PSK}" -ge 8 ] || die "VPN_IPSEC_PSK must be >= 8 chars."
# L2TP pool: explicit local/range, consistency-checked against the /24 net.
# (No suffix-derivation of the base octets: % with dots on both sides of
# the wildcard strips to the first full match, yielding the /16 base
# (10.10) instead of the /24 (10.10.11). Explicit vars plus the prefix
# check below make a mis-pool impossible.)
L2TP_LOCAL="$VPN_L2TP_LOCAL"
L2TP_RANGE="$VPN_L2TP_RANGE"
case "$VPN_L2TP_NET" in
  *.*.*.0/24) ;;
  *) die "VPN_L2TP_NET must be a /24 like 10.10.11.0/24 (got '$VPN_L2TP_NET')." ;;
esac
case "$L2TP_LOCAL" in
  *.*.*.*) ;;
  *) die "VPN_L2TP_LOCAL must be an IPv4 address (got '$L2TP_LOCAL')." ;;
esac
case "$L2TP_LOCAL" in
  *[!0-9.]*) die "VPN_L2TP_LOCAL allows only digits/dots (got '$L2TP_LOCAL')." ;;
esac
case "$L2TP_RANGE" in
  *.*.*.*-*.*.*.*) ;;
  *) die "VPN_L2TP_RANGE must be IP-IP like 10.10.11.10-10.10.11.100." ;;
esac
case "$L2TP_RANGE" in
  *[!0-9.-]*) die "VPN_L2TP_RANGE allows only digits/dots/dash." ;;
esac
# first-three-octets of an IP/CIDR using only single-ended expansions
pfx3() {
  _p="${1%%/*}"
  _p1="${_p%%.*}"; _p="${_p#*.}"
  _p2="${_p%%.*}"; _p="${_p#*.}"
  _p3="${_p%%.*}"
  printf '%s.%s.%s' "$_p1" "$_p2" "$_p3"
}
NET_PFX="$(pfx3 "$VPN_L2TP_NET")"
case "$NET_PFX" in
  *.*.*) ;;
  *) die "Cannot parse VPN_L2TP_NET prefix (net='$VPN_L2TP_NET' pfx='$NET_PFX')." ;;
esac
case "$NET_PFX" in
  *[!0-9.]*) die "Cannot parse VPN_L2TP_NET prefix (net='$VPN_L2TP_NET' pfx='$NET_PFX')." ;;
esac
case "$L2TP_LOCAL" in
  "$NET_PFX".*) ;;
  *) die "VPN_L2TP_LOCAL ($L2TP_LOCAL) is outside VPN_L2TP_NET ($VPN_L2TP_NET)." ;;
esac
case "$L2TP_RANGE" in
  "$NET_PFX".*-"$NET_PFX".*) ;;
  *) die "VPN_L2TP_RANGE ($L2TP_RANGE) is outside VPN_L2TP_NET ($VPN_L2TP_NET)." ;;
esac
# PPTP pool: same explicit local/range discipline as L2TP (always validated
# so a typo can never silently shrink the pool, even while PPTP is off).
case "$VPN_PPTP_NET" in
  *.*.*.0/24) ;;
  *) die "VPN_PPTP_NET must be a /24 like 10.10.12.0/24 (got '$VPN_PPTP_NET')." ;;
esac
case "$VPN_PPTP_LOCAL" in
  *.*.*.*) ;;
  *) die "VPN_PPTP_LOCAL must be an IPv4 address (got '$VPN_PPTP_LOCAL')." ;;
esac
case "$VPN_PPTP_LOCAL" in
  *[!0-9.]*) die "VPN_PPTP_LOCAL allows only digits/dots (got '$VPN_PPTP_LOCAL')." ;;
esac
case "$VPN_PPTP_RANGE" in
  *.*.*.*-*.*.*.*) ;;
  *) die "VPN_PPTP_RANGE must be IP-IP like 10.10.12.10-10.10.12.100." ;;
esac
case "$VPN_PPTP_RANGE" in
  *[!0-9.-]*) die "VPN_PPTP_RANGE allows only digits/dots/dash." ;;
esac
PPTP_PFX="$(pfx3 "$VPN_PPTP_NET")"
case "$PPTP_PFX" in
  *.*.*) ;;
  *) die "Cannot parse VPN_PPTP_NET prefix (net='$VPN_PPTP_NET' pfx='$PPTP_PFX')." ;;
esac
case "$PPTP_PFX" in
  *[!0-9.]*) die "Cannot parse VPN_PPTP_NET prefix (net='$VPN_PPTP_NET' pfx='$PPTP_PFX')." ;;
esac
case "$VPN_PPTP_LOCAL" in
  "$PPTP_PFX".*) ;;
  *) die "VPN_PPTP_LOCAL ($VPN_PPTP_LOCAL) is outside VPN_PPTP_NET ($VPN_PPTP_NET)." ;;
esac
case "$VPN_PPTP_RANGE" in
  "$PPTP_PFX".*-"$PPTP_PFX".*) ;;
  *) die "VPN_PPTP_RANGE ($VPN_PPTP_RANGE) is outside VPN_PPTP_NET ($VPN_PPTP_NET)." ;;
esac
# PPTP on/off flag (parsed once, used for render + firewall + daemon).
case "${VPN_ENABLE_PPTP:-0}" in
  1|true|True|TRUE|yes|Yes|YES|on|On|ON) ENABLE_PPTP=1 ;;
  0|false|False|FALSE|no|No|NO|off|Off|OFF|"") ENABLE_PPTP=0 ;;
  *) die "VPN_ENABLE_PPTP must be 0 or 1 (got '$VPN_ENABLE_PPTP')." ;;
esac
# Template values go through sed: restrict charsets so any input is safe.
case "$VPN_DOMAIN" in
  *[!A-Za-z0-9.-]*) die "VPN_DOMAIN allows only letters/digits/dot/dash (got '$VPN_DOMAIN')." ;;
  "") die "VPN_DOMAIN must be non-empty." ;;
esac
case "$VPN_SUBNET" in *[!0-9./]*) die "VPN_SUBNET must be CIDR (got '$VPN_SUBNET')." ;; esac
case "$VPN_DNS" in *[!0-9.,]*) die "VPN_DNS must be comma-separated IPs (got '$VPN_DNS')." ;; esac

# ---- certs: reuse persisted ones or (re)generate ----
# A trust anchor MUST carry CA:TRUE or strict clients (and strongSwan's
# authority loader) reject it. The server cert MUST carry the domain in
# the SAN with the right type (IP: for IPs, DNS: for names) or IKEv2
# identity binding fails even with a valid chain. Anything off -> regen.
san_entry() {
  case "$1" in
    *.*.*.*)
      case "$1" in *[!0-9.]*) printf 'DNS:%s' "$1" ;; *) printf 'IP:%s' "$1" ;; esac
      ;;
    *) printf 'DNS:%s' "$1" ;;
  esac
}
want_san() {
  # openssl text form: "IP Address:1.2.3.4" / "DNS:name"
  case "$(san_entry "$1")" in IP:*) printf 'IP Address:%s' "$1" ;; *) printf 'DNS:%s' "$1" ;; esac
}
certs_ok() {
  [ -f "$PERSIST_DIR/ca.crt" ] && [ -f "$PERSIST_DIR/server.crt" ] && [ -f "$PERSIST_DIR/server.key" ] \
    && openssl x509 -in "$PERSIST_DIR/ca.crt" -noout -text 2>/dev/null | grep -q "CA:TRUE" \
    && openssl verify -CAfile "$PERSIST_DIR/ca.crt" "$PERSIST_DIR/server.crt" >/dev/null 2>&1 \
    && openssl x509 -in "$PERSIST_DIR/server.crt" -noout -ext subjectAltName 2>/dev/null | grep -qF "$(want_san "$VPN_DOMAIN")"
}
if certs_ok; then
  log "Reusing persisted certs from $PERSIST_DIR"
else
  if [ -f "$PERSIST_DIR/ca.crt" ]; then
    log "WARN: existing CA unusable (missing CA:TRUE or bad chain) - regenerating all certs."
    mv -f "$PERSIST_DIR/ca.crt" "$PERSIST_DIR/ca.crt.bak" 2>/dev/null || true
  fi
  log "Generating CA + server cert for $VPN_DOMAIN (this happens once)..."
  cat > /tmp/ca.cnf <<'EOF'
[req]
distinguished_name = dn
[dn]
[v3_ca]
basicConstraints = critical, CA:TRUE
keyUsage = critical, keyCertSign, cRLSign, digitalSignature
subjectKeyIdentifier = hash
EOF
  openssl req -x509 -newkey rsa:3072 -sha256 -days 825 \
    -nodes -keyout "$PERSIST_DIR/ca.key" -out "$PERSIST_DIR/ca.crt" \
    -subj "/CN=V2ProDock VPN CA" -config /tmp/ca.cnf -extensions v3_ca 2>/dev/null \
    || die "openssl CA generation failed"
  rm -f /tmp/ca.cnf
  chmod 600 "$PERSIST_DIR/ca.key"
  openssl req -newkey rsa:3072 -nodes -keyout "$PERSIST_DIR/server.key" \
    -out /tmp/server.csr -subj "/CN=${VPN_DOMAIN}" 2>/dev/null \
    || die "openssl server key generation failed"
  chmod 600 "$PERSIST_DIR/server.key"
  # SAN: primary domain (typed) + extras (e.g. VPS public IP + LAN IP).
  SAN_LIST="$(san_entry "$VPN_DOMAIN")"
  OLD_SAN_IFS="$IFS"; IFS=','
  # shellcheck disable=SC2162
  for extra in $VPN_EXTRA_SANS; do
    extra="$(echo "$extra" | tr -d ' \t\r\n')"
    [ -n "$extra" ] || continue
    case "$extra" in *[!A-Za-z0-9.-]*) die "VPN_EXTRA_SANS allows only hostnames/IPs (got '$extra')." ;; esac
    SAN_LIST="${SAN_LIST},$(san_entry "$extra")"
  done
  IFS="$OLD_SAN_IFS"
  printf 'subjectAltName = %s\nbasicConstraints = CA:FALSE\nkeyUsage = digitalSignature,keyEncipherment\nextendedKeyUsage = serverAuth\n' \
    "$SAN_LIST" > /tmp/server.ext
  openssl x509 -req -in /tmp/server.csr -CA "$PERSIST_DIR/ca.crt" \
    -CAkey "$PERSIST_DIR/ca.key" -CAcreateserial -days 825 -sha256 \
    -extfile /tmp/server.ext -out "$PERSIST_DIR/server.crt" 2>/dev/null \
    || die "openssl server cert signing failed"
  rm -f /tmp/server.csr /tmp/server.ext "$PERSIST_DIR/ca.srl"
  log "Certs generated. Install ca.crt on legacy devices (iOS/tvOS need the profile)."
fi

cp -f "$PERSIST_DIR/ca.crt" /etc/swanctl/x509ca/ca.crt
cp -f "$PERSIST_DIR/server.crt" /etc/swanctl/x509/server.crt
cp -f "$PERSIST_DIR/server.key" /etc/swanctl/private/server.key
chmod 600 /etc/swanctl/private/server.key
log "Certs installed into swanctl store."
# Apple profile for legacy devices (best-effort, never fatal to VPN boot).
if [ -x /gen-mobileconfig.sh ]; then
  OUT="$PERSIST_DIR/apple.mobileconfig" PERSIST_DIR="$PERSIST_DIR" VPN_DOMAIN="$VPN_DOMAIN" /gen-mobileconfig.sh 2>&1 || \
    log "WARN: mobileconfig generation failed (VPN still boots)."
fi

# ---- render swanctl.conf ----
# Secrets are written to temp files and spliced in by LINE (awk file read),
# so passwords/PSK may contain any characters except " and \ (rejected below).
: > /tmp/eap-secrets.conf
EAP_COUNT=0
OLD_IFS="$IFS"; IFS=','
# shellcheck disable=SC2162
for pair in $VPN_USERS; do
  pair="$(echo "$pair" | tr -d ' \t\r\n')"
  [ -n "$pair" ] || continue
  u="${pair%%:*}"; p="${pair#*:}"
  [ -n "$u" ] && [ -n "$p" ] && [ "$u" != "$pair" ] || die "Bad VPN_USERS entry (want user:pass)."
  [ "${#p}" -ge 8 ] || die "Password for a user must be >= 8 chars."
  case "$u" in *[:\"\\,]*|"") die "Username allows no ': \" \\ ,' or empties." ;; esac
  case "$p" in *[\"\\]*) die "Passwords with '\"' or '\\' are not supported (swanctl quoting)." ;; esac
  EAP_COUNT=$((EAP_COUNT + 1))
  printf '  eap-user%d {\n    id = %s\n    secret = "%s"\n  }\n' "$EAP_COUNT" "$u" "$p" >> /tmp/eap-secrets.conf
done
IFS="$OLD_IFS"
[ "$EAP_COUNT" -gt 0 ] || die "No valid EAP users parsed."
case "$VPN_IPSEC_PSK" in *[\"\\]*) die "VPN_IPSEC_PSK with '\"' or '\\' is not supported (swanctl quoting)." ;; esac
printf '  ike-l2tp {\n    secret = "%s"\n  }\n' "$VPN_IPSEC_PSK" > /tmp/psk-secret.conf

sed -e "s|__VPN_DOMAIN__|${VPN_DOMAIN}|g" \
    -e "s|__VPN_SUBNET__|${VPN_SUBNET}|g" \
    -e "s|__VPN_DNS__|${VPN_DNS}|g" \
    /swanctl.conf.tmpl > /tmp/swanctl.rendered
# splice secret files at marker lines (literal print: immune to & \ in secrets)
awk '
  /# __EAP_USERS__/ { while ((getline l < "/tmp/eap-secrets.conf") > 0) print l; next }
  /# __IPSEC_PSK__/ { while ((getline l < "/tmp/psk-secret.conf") > 0) print l; next }
  { print }
' /tmp/swanctl.rendered > "$SWANCTL_CONF"
grep -q "id = " "$SWANCTL_CONF" || die "swanctl render failed: no EAP users"
grep -q "ike-l2tp" "$SWANCTL_CONF" || die "swanctl render failed: no L2TP PSK"
rm -f /tmp/eap-secrets.conf /tmp/psk-secret.conf /tmp/swanctl.rendered
log "swanctl.conf rendered: domain=$VPN_DOMAIN ikev2-pool=$VPN_SUBNET l2tp-psk armed, $EAP_COUNT EAP user(s)."

# ---- kernel forwarding (compose sysctls is the supported path) ----
cur_fwd="$(cat /proc/sys/net/ipv4/ip_forward 2>/dev/null || echo 0)"
if [ "$cur_fwd" = "1" ]; then
  log "ip_forward=1 already (compose sysctls)."
elif echo 1 > /proc/sys/net/ipv4/ip_forward 2>/dev/null; then
  log "ip_forward enabled."
else
  log "WARN: ip_forward=$cur_fwd and /proc is read-only here; ensure compose sysctls net.ipv4.ip_forward=1 is set. Continuing (charon/iptables still validated)."
fi

# ---- L2TP configs (xl2tpd + pppd + chap-secrets) ----
sed -e "s|__L2TP_RANGE__|${L2TP_RANGE}|g" \
    -e "s|__L2TP_LOCAL__|${L2TP_LOCAL}|g" \
    /xl2tpd.conf.tmpl > /etc/xl2tpd/xl2tpd.conf
DNS1="${VPN_DNS%%,*}"; DNS_REST="${VPN_DNS#*,}"
[ "$DNS_REST" = "$VPN_DNS" ] && DNS_REST=""
{
  printf 'ms-dns %s\n' "$DNS1"
  [ -n "$DNS_REST" ] && printf 'ms-dns %s\n' "${DNS_REST%%,*}"
} > /tmp/ms-dns.conf
awk '
  /# __MS_DNS__/ { while ((getline l < "/tmp/ms-dns.conf") > 0) print l; next }
  { print }
' /options.xl2tpd.tmpl > /etc/ppp/options.xl2tpd
rm -f /tmp/ms-dns.conf
grep -q "ms-dns $DNS1" /etc/ppp/options.xl2tpd || die "ppp options render failed (ms-dns)."
: > /etc/ppp/chap-secrets
chmod 600 /etc/ppp/chap-secrets
OLD_IFS="$IFS"; IFS=','
# shellcheck disable=SC2162
for pair in $VPN_USERS; do
  pair="$(echo "$pair" | tr -d ' \t\r\n')"
  [ -n "$pair" ] || continue
  u="${pair%%:*}"; p="${pair#*:}"
  printf '"%s" * "%s" *\n' "$u" "$p" >> /etc/ppp/chap-secrets
done
IFS="$OLD_IFS"
[ -s /etc/ppp/chap-secrets ] || die "chap-secrets render failed."
log "L2TP rendered: lns=$L2TP_LOCAL range=$L2TP_RANGE net=$VPN_L2TP_NET ($EAP_COUNT chap user(s))."

# ---- PPTP configs (pptpd + pppd options) ----
# chap-secrets above is shared: pptpd reads the same file, so the same
# VPN_USERS creds log in via IKEv2, L2TP and PPTP alike.
if [ "$ENABLE_PPTP" = "1" ]; then
  mkdir -p /etc/pptpd
  # poptop only parses SHORT ranges (startIP-lastOctet, e.g. 10.10.12.10-100):
  # a full IP-IP value is misread as a DNS name, NXDOMAINs, and pptpd exits 1
  # with the error going to syslog only (proven via strace). The /24-prefix
  # validation above guarantees both ends share the first three octets, so
  # the short form is always exact.
  PPTP_START="${VPN_PPTP_RANGE%%-*}"
  PPTP_END_LAST="${VPN_PPTP_RANGE##*.}"
  PPTP_SHORT_RANGE="${PPTP_START}-${PPTP_END_LAST}"
  sed -e "s|__PPTP_RANGE__|${PPTP_SHORT_RANGE}|g" \
      -e "s|__PPTP_LOCAL__|${VPN_PPTP_LOCAL}|g" \
      /pptpd.conf.tmpl > /etc/pptpd/pptpd.conf
  grep -q "localip $VPN_PPTP_LOCAL" /etc/pptpd/pptpd.conf || die "pptpd.conf render failed (localip)."
  grep -q "remoteip $PPTP_SHORT_RANGE" /etc/pptpd/pptpd.conf || die "pptpd.conf render failed (remoteip)."
  DNS1="${VPN_DNS%%,*}"; DNS_REST="${VPN_DNS#*,}"
  [ "$DNS_REST" = "$VPN_DNS" ] && DNS_REST=""
  {
    printf 'ms-dns %s\n' "$DNS1"
    [ -n "$DNS_REST" ] && printf 'ms-dns %s\n' "${DNS_REST%%,*}"
  } > /tmp/ms-dns-pptp.conf
  awk '
    /# __MS_DNS__/ { while ((getline l < "/tmp/ms-dns-pptp.conf") > 0) print l; next }
    { print }
  ' /options.pptpd.tmpl > /etc/ppp/options.pptpd
  rm -f /tmp/ms-dns-pptp.conf
  grep -q "ms-dns $DNS1" /etc/ppp/options.pptpd || die "pptp ppp options render failed (ms-dns)."
  grep -q "require-mppe-128" /etc/ppp/options.pptpd || die "pptp ppp options must require MPPE-128."
  # Session hooks must be present AND executable or pppd misbehaves on
  # every connect (pppd warns and continues, but the reconnect telemetry
  # this was built for would silently go missing - fail fast instead).
  [ -x /etc/ppp/pptp-ip-up ] || die "pptp ip-up hook missing/not executable."
  [ -x /etc/ppp/pptp-ip-down ] || die "pptp ip-down hook missing/not executable."
  log "PPTP rendered: local=$VPN_PPTP_LOCAL range=$VPN_PPTP_RANGE net=$VPN_PPTP_NET (MPPE-128 mandatory, chap-secrets shared)."
fi

# ---- policy routing exceptions: traffic TO a VPN pool address must use
# real interfaces (ppp0/XFRM), never the tunnel. Without this, e.g. ping
# replies to the LNS address or inter-client traffic get src-matched into
# table 100 and die in hev (proven in E2E). Deterministic order: 217 here,
# 218/219 from-rules in watchdog (lower number = higher precedence).
# Fail-closed is unaffected: internet destinations never match these rules.
for _pool in "$VPN_SUBNET" "$VPN_L2TP_NET" "$VPN_PPTP_NET"; do
  ip rule show 2>/dev/null | grep -q "to ${_pool} lookup main" \
    || ip rule add to "$_pool" lookup main pref 217 2>/dev/null \
    || log "WARN: cannot add to-pool routing exception for $_pool"
done

# ---- iptables: VPN clients may ONLY leave via tun0 (never direct) ----
# Idempotent (-C checks first). Applies to BOTH the IKEv2 and L2TP pools.
VPN_NET="$VPN_SUBNET"
enforce_net() {
  _net="$1"
  iptables -C FORWARD -s "$_net" -o "$TUN_DEV" -j ACCEPT 2>/dev/null || iptables -A FORWARD -s "$_net" -o "$TUN_DEV" -j ACCEPT
  iptables -C FORWARD -s "$_net" -j DROP 2>/dev/null || iptables -A FORWARD -s "$_net" -j DROP
  # NOTE: NO MASQUERADE here on purpose. hev-socks5-tunnel is a userspace L3
  # endpoint that maps flows itself; NATing client sources to the TUN address
  # makes replies come back addressed to the gateway itself, where they die
  # in INPUT instead of being forwarded back through IPsec (proven in E2E).
  # Any MASQUERADE for these nets left from older builds is removed:
  iptables -t nat -D POSTROUTING -s "$_net" -o "$TUN_DEV" -j MASQUERADE 2>/dev/null || true
  log "iptables enforced: $_net -> $TUN_DEV only, un-NATed (direct egress DROPPED)."
}
iptables -C INPUT -p udp --dport 500 -j ACCEPT 2>/dev/null || iptables -A INPUT -p udp --dport 500 -j ACCEPT
iptables -C INPUT -p udp --dport 4500 -j ACCEPT 2>/dev/null || iptables -A INPUT -p udp --dport 4500 -j ACCEPT
# Bare L2TP (no IPsec) is cleartext: refused by default, opt-in only.
# Enforcement needs xt_policy; without it we can only warn (PPP auth still
# gates access, but encryption is not guaranteed).
case "${VPN_ALLOW_PLAIN_L2TP:-0}" in
  1|true|True|TRUE|yes|Yes|YES|on|On|ON) ALLOW_PLAIN_L2TP=1 ;;
  0|false|False|FALSE|no|No|NO|off|Off|OFF|"") ALLOW_PLAIN_L2TP=0 ;;
  *) die "VPN_ALLOW_PLAIN_L2TP must be 0 or 1 (got '$VPN_ALLOW_PLAIN_L2TP')." ;;
esac
if [ "$ALLOW_PLAIN_L2TP" = "0" ]; then
  if iptables -C INPUT -p udp --dport 1701 -m policy --dir in --pol none -j DROP 2>/dev/null; then
    log "L2TP locked to IPsec (bare L2TP dropped)."
  elif iptables -A INPUT -p udp --dport 1701 -m policy --dir in --pol none -j DROP 2>/dev/null; then
    log "L2TP locked to IPsec (bare L2TP dropped)."
  else
    log "WARN: xt_policy unavailable - cannot refuse bare L2TP at packet level. L2TP logins still need PPP creds, but set VPN_ALLOW_PLAIN_L2TP consciously."
  fi
else
  iptables -D INPUT -p udp --dport 1701 -m policy --dir in --pol none -j DROP 2>/dev/null || true
  log "WARN: VPN_ALLOW_PLAIN_L2TP=1 - bare L2TP ACCEPTED. PPP logins AND all user traffic cross the internet in CLEARTEXT. Use only for routers that cannot do IPsec, with a unique strong password."
fi
iptables -C INPUT -p udp --dport 1701 -j ACCEPT 2>/dev/null || iptables -A INPUT -p udp --dport 1701 -j ACCEPT
# PPTP control channel (TCP 1723). Only opened when PPTP is enabled; GRE
# (IP proto 47) is a separate protocol - Docker ports: cannot publish it,
# so on Linux the host needs nf_conntrack_pptp (install.sh modprobes it)
# or host networking, otherwise control connects but data stalls.
if [ "$ENABLE_PPTP" = "1" ]; then
  iptables -C INPUT -p tcp --dport 1723 -j ACCEPT 2>/dev/null || iptables -A INPUT -p tcp --dport 1723 -j ACCEPT
  iptables -C INPUT -p gre -j ACCEPT 2>/dev/null || iptables -A INPUT -p gre -j ACCEPT
  log "WARN: VPN_ENABLE_PPTP=1 - PPTP is cryptographically BROKEN (MSCHAPv2/MPPE). Keep the PPTP leg on a trusted LAN only; internet egress stays via the working proxy. MPPE-128 is mandatory."
else
  iptables -D INPUT -p tcp --dport 1723 -j ACCEPT 2>/dev/null || true
  log "PPTP disabled (VPN_ENABLE_PPTP=0): TCP 1723 not served."
fi
# Return path: hev terminates client flows in userspace and re-injects
# replies into tun0, so conntrack never sees them as ESTABLISHED (a state
# match here would blackhole ALL client return traffic - proven in E2E).
# Plain ACCEPT is safe: hev only emits replies to flows it opened toward
# the SOCKS upstream, and everything still funnels via that upstream.
iptables -C FORWARD -i "$TUN_DEV" -j ACCEPT 2>/dev/null \
  || iptables -A FORWARD -i "$TUN_DEV" -j ACCEPT
enforce_net "$VPN_SUBNET"
enforce_net "$VPN_L2TP_NET"
# PPTP pool gets the same fail-closed treatment (only via tun0, never
# direct) - but only when PPTP is served; otherwise no such traffic exists.
if [ "$ENABLE_PPTP" = "1" ]; then
  enforce_net "$VPN_PPTP_NET"
fi

# ---- start charon via distro starter (charon lives under libexec on Alpine) ----
mkdir -p /var/run/charon /var/log
rm -f /var/run/charon/charon-vici.sock
log "Starting IPsec (starter)..."
ipsec start > /var/log/ipsec-start.log 2>&1 || { cat /var/log/ipsec-start.log 2>/dev/null; die "ipsec start failed"; }
sleep 3
ipsec status >/dev/null 2>&1 || swanctl --list-conns >/dev/null 2>&1 || {
  cat /var/log/ipsec-start.log 2>/dev/null; die "charon failed to start (ipsec status + swanctl both down)"
}
swanctl --load-all --noprompt > /tmp/swanctl-load.log 2>&1 || { cat /tmp/swanctl-load.log; die "swanctl --load-all failed"; }
# NOTE: never `tee ... | grep -q` here: grep -q exits on first match and
# SIGPIPE-truncates the file, hiding later conns from subsequent checks.
swanctl --list-conns > /tmp/swanctl-conns.log 2>&1 || die "swanctl --list-conns failed"
grep -q "ikev2-eap" /tmp/swanctl-conns.log || die "ikev2-eap conn not loaded"
grep -q "l2tp-psk" /tmp/swanctl-conns.log || die "l2tp-psk conn not loaded"
log "charon up, ikev2-eap + l2tp-psk loaded."

mkdir -p /run/ppp
# ---- /dev/ppp: pppd cannot run without it (PPPIOCNEWUNIT) ----
# Docker does not provide it: creating the node needs CAP_MKNOD, OPENING it
# needs cgroup permission (compose device_cgroup_rules), and the host kernel
# needs ppp support (most VPS: modprobe ppp_generic). Existence is NOT enough
# (open can still be denied), so the node is open-tested, not just stat'ed.
if [ ! -c /dev/ppp ]; then
  mknod /dev/ppp c 108 0 2>/dev/null || true
fi
: > /dev/ppp 2>/dev/null || die "cannot open /dev/ppp: need compose device_cgroup_rules 'c 108:0 rwm' + host ppp support (modprobe ppp_generic)."

# ---- start xl2tpd (L2TP LNS on UDP 1701) ----
mkdir -p /var/run/xl2tpd
# Stale pid/control files survive `docker restart` (same fs) and would block
# xl2tpd forever ("already running" while pointing at a dead pid).
rm -f /var/run/xl2tpd.pid /var/run/xl2tpd/xl2tpd.pid /var/run/xl2tpd/xl2tpd-control
log "Starting xl2tpd..."
xl2tpd -D > /var/log/xl2tpd.log 2>&1 &
XL2TPD_PID=$!
sleep 2
kill -0 "$XL2TPD_PID" 2>/dev/null || { cat /var/log/xl2tpd.log 2>/dev/null; die "xl2tpd failed to start"; }
netstat -uln 2>/dev/null | grep -q ":1701 " || die "xl2tpd not listening on UDP 1701"
log "xl2tpd up on UDP 1701 (L2TP range $L2TP_RANGE)."

# ---- start pptpd (PPTP on TCP 1723 + GRE proto 47), opt-in only ----
if [ "$ENABLE_PPTP" = "1" ]; then
  rm -f /var/run/pptpd.pid
  log "Starting pptpd (TCP 1723, MPPE-128 mandatory)..."
  pptpd -c /etc/pptpd/pptpd.conf -o /etc/ppp/options.pptpd -f > /var/log/pptpd.log 2>&1 &
  PPTPD_PID=$!
  sleep 2
  kill -0 "$PPTPD_PID" 2>/dev/null || { cat /var/log/pptpd.log 2>/dev/null; die "pptpd failed to start"; }
  netstat -tln 2>/dev/null | grep -q ":1723 " || die "pptpd not listening on TCP 1723"
  log "pptpd up on TCP 1723 (PPTP range $VPN_PPTP_RANGE). Same-LAN clients only; GRE needs host nf_conntrack_pptp behind Docker NAT."
fi

# ---- hand over to watchdog (owns tun0 + upstream pinning + proofs) ----
export VPN_NET VPN_L2TP_NET VPN_PPTP_NET ENABLE_PPTP TUN_DEV TUN_ADDR TUN_MTU V2PRODOCK_API V2PRODOCK_SOCKS_HOST PERSIST_DIR VPN_DOMAIN ALLOW_PLAIN_L2TP
exec /watchdog.sh
