#!/bin/sh
# watchdog: pins tun0->SOCKS to the fastest ALIVE Xray config and proves
# VPN egress == that config's egress. Never routes VPN direct.
set -eu

# Millisecond timestamps on every line (docker logs without --timestamps
# shows nothing otherwise; matches the Go logger's %H:%M:%S.%3N shape).
# Portable: GNU date prints %N natively, busybox too; anything else falls
# back to .000 instead of leaking a literal %N into the logs.
ts() {
  _s="$(date '+%Y-%m-%d %H:%M:%S')"
  _n="$(date '+%N' 2>/dev/null)"
  case "$_n" in ''|*[!0-9]*) _n="000000000" ;; esac
  printf '%s.%03d' "$_s" "$((10#$_n / 1000000))"
}
log() { echo "$(ts) [v2prodock-vpn] $*"; }
warn() { echo "$(ts) [v2prodock-vpn][WARN] $*" >&2; }

V2PRODOCK_API="${V2PRODOCK_API:-http://v2prodock:27018/proxies}"
V2PRODOCK_SOCKS_HOST="${V2PRODOCK_SOCKS_HOST:-v2prodock}"
TUN_DEV="${TUN_DEV:-tun0}"
TUN_ADDR="${TUN_ADDR:-198.18.0.1}"
TUN_MTU="${TUN_MTU:-1500}"
VPN_NET="${VPN_NET:-10.10.10.0/24}"
VPN_L2TP_NET="${VPN_L2TP_NET:-10.10.11.0/24}"
VPN_PPTP_NET="${VPN_PPTP_NET:-10.10.12.0/24}"
ENABLE_PPTP="${ENABLE_PPTP:-0}"
ALLOW_PLAIN_L2TP="${ALLOW_PLAIN_L2TP:-0}"
POLL_SECS="${POLL_SECS:-15}"
STATUS_FILE="${STATUS_FILE:-/config/vpn/status.json}"
EGRESS_URL="${EGRESS_URL:-https://api.ipify.org}"
HEV_BIN="${HEV_BIN:-hev-socks5-tunnel}"
HEV_CONF="${HEV_CONF:-/tmp/hev.yaml}"

HEV_PID=""
CURRENT_SOCKS=""
# Log-noise control: steady states NEVER log per cycle. VERIFIED logs only
# when the proven (upstream, IP) pair CHANGES or recovers; failures log once
# on transition (0->1) plus one heartbeat every PROOF_HEARTBEAT_Nth while
# still down (status.json is rewritten every poll regardless, so the API is
# always current). A healthy tunnel at POLL_SECS=15 would otherwise log ~6k
# identical lines a day; a dead one did exactly that before this policy.
LAST_VERIFIED=""
PROOF_FAILS=0
API_FAILS=0
TUN_FAILS=0
PROOF_HEARTBEAT_N="${PROOF_HEARTBEAT_N:-20}"
# A 0/garbage heartbeat would divide by zero under set -eu and kill the
# gateway; degrade to the default instead.
case "$PROOF_HEARTBEAT_N" in ''|*[!0-9]*|0) PROOF_HEARTBEAT_N=20 ;; esac

cleanup() {
  log "Shutting down tunnel + VPN daemons..."
  if [ -n "$HEV_PID" ] && kill -0 "$HEV_PID" 2>/dev/null; then kill "$HEV_PID" 2>/dev/null || true; fi
  pkill -f "$HEV_BIN" 2>/dev/null || true
  ipsec stop 2>/dev/null || true
  pkill -x xl2tpd 2>/dev/null || true
  pkill -x pptpd 2>/dev/null || true
  exit 0
}
trap cleanup TERM INT

write_status() {
  # $1 upstream_socks $2 upstream_name $3 egress_ip $4 verified(true/false) $5 error
  mkdir -p "$(dirname "$STATUS_FILE")" 2>/dev/null || true
  ts="$(date -u +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date)"
  verified="$4"; err="$5"
  # minimal JSON escaping (names are subscription remarks; strip quotes/newlines)
  name_clean="$(printf '%s' "$2" | tr -d '"\\' | tr '\n\r' '  ' | cut -c1-80)"
  err_clean="$(printf '%s' "$err" | tr -d '"\\' | tr '\n\r' '  ' | cut -c1-200)"
  _plain=false
  [ "${ALLOW_PLAIN_L2TP:-0}" = "1" ] && _plain=true
  _pptp=false
  [ "${ENABLE_PPTP:-0}" = "1" ] && _pptp=true
  printf '{"updated":"%s","upstream_socks":"%s","upstream_name":"%s","tun":"%s","egress_ip":"%s","verified":%s,"error":"%s","ikev2":true,"l2tp":true,"plain_l2tp":%s,"pptp":%s}\n' \
    "$ts" "$1" "$name_clean" "$TUN_DEV" "$3" "$verified" "$err_clean" "$_plain" "$_pptp" > "$STATUS_FILE.tmp" 2>/dev/null \
    && mv -f "$STATUS_FILE.tmp" "$STATUS_FILE" 2>/dev/null || true
}

# Prints "host:port|name" for fastest alive proxy, or fails.
api_fastest() {
  body="$(curl -fsS --max-time 8 "$V2PRODOCK_API" 2>/dev/null)" || return 1
  socks_raw="$(printf '%s' "$body" | jq -r '.[0].socks5 // empty' 2>/dev/null)" || return 1
  name="$(printf '%s' "$body" | jq -r '.[0].name // "unknown"' 2>/dev/null)"
  [ -n "$socks_raw" ] && [ "$socks_raw" != "null" ] || return 1
  port="${socks_raw##*:}"
  case "$port" in ''|*[!0-9]*) return 1 ;; esac
  printf '%s:%s|%s' "$V2PRODOCK_SOCKS_HOST" "$port" "$name"
}

render_hev() {
  # $1 socks_host $2 socks_port
  cat > "$HEV_CONF" <<EOF
tunnel:
  name: ${TUN_DEV}
  mtu: ${TUN_MTU}
  ipv4: ${TUN_ADDR}
socks5:
  address: $1
  port: $2
  udp: udp
misc:
  task-stack-size: 20480
  connect-timeout: 5000
  read-write-timeout: 60000
  log-level: warn
  limit-nofile: 65535
EOF
}

tunnel_alive() {
  ip link show "$TUN_DEV" 2>/dev/null | grep -q "UP" || return 1
  [ -n "$HEV_PID" ] && kill -0 "$HEV_PID" 2>/dev/null
}

start_tunnel() {
  # $1 host:port $2 name [$3 loud|quiet, default loud]
  # quiet is for the crash-loop restart path: retrying still happens every
  # poll, but Pinning/Tunnel-up lines don't (recovery shows up via VERIFIED).
  socks="$1"; name="$2"; mode="${3:-loud}"
  note() {
    if [ "$mode" = "loud" ]; then log "$1"; fi
  }
  host="${socks%%:*}"; port="${socks##*:}"
  note "Pinning $TUN_DEV -> SOCKS $socks ($name) ..."
  render_hev "$host" "$port"
  if [ -n "$HEV_PID" ] && kill -0 "$HEV_PID" 2>/dev/null; then kill "$HEV_PID" 2>/dev/null || true; sleep 1; fi
  pkill -f "$HEV_BIN $HEV_CONF" 2>/dev/null || true
  "$HEV_BIN" "$HEV_CONF" > /var/log/hev.log 2>&1 &
  HEV_PID=$!
  # wait for TUN device
  i=0
  while [ "$i" -lt 20 ]; do
    ip link show "$TUN_DEV" >/dev/null 2>&1 && break
    sleep 0.5; i=$((i + 1))
  done
  ip link show "$TUN_DEV" >/dev/null 2>&1 || { tail -20 /var/log/hev.log 2>/dev/null; return 1; }
  ip addr replace "${TUN_ADDR}/16" dev "$TUN_DEV" 2>/dev/null || true
  ip link set "$TUN_DEV" up 2>/dev/null || true
  ip link set "$TUN_DEV" mtu "$TUN_MTU" 2>/dev/null || true
  # Source-based routing: ONLY VPN client subnets (IKEv2 + L2TP) use tun0.
  # Gateway's own SOCKS connection (src=gw IP) stays on main table -> no loop.
  ip rule show | grep -q "from ${VPN_NET} lookup 100" 2>/dev/null \
    || ip rule add from "$VPN_NET" table 100 pref 218 2>/dev/null || warn "ip rule failed (needs NET_ADMIN)"
  if [ -n "${VPN_L2TP_NET:-}" ]; then
    ip rule show | grep -q "from ${VPN_L2TP_NET} lookup 100" 2>/dev/null \
      || ip rule add from "$VPN_L2TP_NET" table 100 pref 219 2>/dev/null || warn "ip rule (l2tp) failed"
  fi
  # Pref 221, NOT 220: Docker installs its own per-network rule at pref 220
  # ("from all lookup <table>") in every container, and same-pref rules lose
  # to it - PPTP traffic would route via eth0 into the fail-closed DROP
  # instead of tun0 (proven by counters in E2E: DROP hit, ACCEPT missed).
  # 217 = to-pool exceptions, 218/219/221 = from-rules, 220 = Docker's.
  if [ "${ENABLE_PPTP:-0}" = "1" ] && [ -n "${VPN_PPTP_NET:-}" ]; then
    ip rule show | grep -q "from ${VPN_PPTP_NET} lookup 100" 2>/dev/null \
      || ip rule add from "$VPN_PPTP_NET" table 100 pref 221 2>/dev/null || warn "ip rule (pptp) failed"
  fi
  ip route replace default dev "$TUN_DEV" table 100 2>/dev/null || warn "ip route table 100 failed"
  CURRENT_SOCKS="$socks"
  if [ "${ENABLE_PPTP:-0}" = "1" ]; then
    note "Tunnel up: $VPN_NET${VPN_L2TP_NET:+, $VPN_L2TP_NET}${VPN_PPTP_NET:+, $VPN_PPTP_NET} --table100--> $TUN_DEV -> $socks (gateway own traffic stays direct, no loop)."
  else
    note "Tunnel up: $VPN_NET${VPN_L2TP_NET:+, $VPN_L2TP_NET} --table100--> $TUN_DEV -> $socks (gateway own traffic stays direct, no loop)."
  fi
  return 0
}

# Egress proof: IP seen THROUGH upstream SOCKS must equal IP seen VIA tun0.
# Prints egress IP on success. An EMPTY tun answer (tunnel stalled, upstream
# UDP-blocked) and a DIFFERING answer (misrouting) are different failures
# with different fixes — logged distinctly, never lumped together.
verify_egress() {
  # $1 host:port [$2 loud|quiet, default loud]
  # rc 0 proven (eip on stdout); 1 socks-side down; 2 MISMATCH; 3 UNPROVEN.
  # loud warns immediately (rare paths: initial pin, re-pin). quiet stays
  # silent (hot periodic path — the loop logs transitions + heartbeat
  # itself, so one stuck tunnel can't fill the log with identical pairs).
  socks="$1"; mode="${2:-loud}"
  note() {
    if [ "$mode" = "loud" ]; then warn "$1"; fi
  }
  via_socks="$(curl -fsS --max-time 8 --socks5-hostname "$socks" "$EGRESS_URL" 2>/dev/null | tr -d ' \r\n')" || {
    note "EGRESS CHECK failed via $socks (pinned upstream SOCKS itself down?)"
    return 1
  }
  [ -n "$via_socks" ] || {
    note "EGRESS CHECK failed via $socks (pinned upstream SOCKS empty answer)"
    return 1
  }
  via_tun="$(curl -fsS --max-time 8 --interface "$TUN_DEV" "$EGRESS_URL" 2>/dev/null | tr -d ' \r\n')" || {
    note "EGRESS UNPROVEN via $socks: tun gave no response (tunnel stalled? upstream UDP-blocked?). Fail-closed: clients have no direct path."
    return 3
  }
  [ -n "$via_tun" ] || {
    note "EGRESS UNPROVEN via $socks: empty tun response."
    return 3
  }
  [ "$via_socks" = "$via_tun" ] || {
    note "EGRESS MISMATCH: socks=$via_socks tun=$via_tun (tunnel egress differs from pinned upstream!)"
    return 2
  }
  printf '%s' "$via_tun"
  return 0
}

# Log a fresh VERIFIED proof only when the (upstream, IP) pair changed or
# recovered — a healthy tunnel at POLL_SECS=15 would otherwise log ~6k
# identical lines a day. Resets the failure streak.
report_verified() {
  # $1 socks $2 name $3 eip
  key="$1|$3"
  if [ "$key" != "$LAST_VERIFIED" ]; then
    log "VPN egress VERIFIED via $2 ($1): $3"
    LAST_VERIFIED="$key"
  fi
  PROOF_FAILS=0
}

log "Waiting for alive Xray proxy at $V2PRODOCK_API ..."
tries=0
fastest=""
while [ "$tries" -lt 60 ]; do
  if fastest="$(api_fastest)"; then break; fi
  tries=$((tries + 1)); sleep 5
done
[ -n "$fastest" ] || { write_status "" "" "" false "no alive proxy from $V2PRODOCK_API after 5min"; exit 1; }

socks="${fastest%%|*}"; pname="${fastest#*|}"
start_tunnel "$socks" "$pname" || exit 1
if eip="$(verify_egress "$socks")"; then
  report_verified "$socks" "$pname" "$eip"
  write_status "$socks" "$pname" "$eip" true ""
else
  warn "Initial egress proof failed (upstream $socks). VPN stays up, retrying in loop; clients have no direct path."
  write_status "$socks" "$pname" "" false "initial egress proof failed"
fi

# charon keeps no config on disk: if it ever restarts, the runtime-loaded
# conns/creds vanish and VPN silently stops terminating. Self-heal.
ensure_ipsec() {
  if ! ipsec status >/dev/null 2>&1; then
    warn "charon/starter down - exiting so Docker restarts the gateway."
    return 1
  fi
  _conns="$(swanctl --list-conns 2>/dev/null)"
  if printf '%s' "$_conns" | grep -q "ikev2-eap" \
    && printf '%s' "$_conns" | grep -q "l2tp-psk"; then
    return 0
  fi
  warn "IPsec runtime state incomplete (charon restarted?) - reloading all."
  swanctl --load-all --noprompt >/dev/null 2>&1 || warn "swanctl reload failed"
  return 0
}

# pptpd has no runtime-state dependency like charon, but if the process
# dies the TCP SYNs from ancient devices get silence (the exact symptom
# that started this). Self-heal by restarting it with the rendered config.
ensure_pptpd() {
  [ "${ENABLE_PPTP:-0}" = "1" ] || return 0
  if netstat -tln 2>/dev/null | grep -q ":1723 "; then
    return 0
  fi
  warn "pptpd not listening on TCP 1723 - restarting."
  rm -f /var/run/pptpd.pid
  pptpd -c /etc/pptpd/pptpd.conf -o /etc/ppp/options.pptpd -f > /var/log/pptpd.log 2>&1 &
  sleep 2
  netstat -tln 2>/dev/null | grep -q ":1723 " || warn "pptpd restart failed (see /var/log/pptpd.log)"
  return 0
}

while true; do
  sleep "$POLL_SECS"
  ensure_ipsec || exit 1
  ensure_pptpd
  if ! fastest="$(api_fastest)"; then
    API_FAILS=$((API_FAILS + 1))
    if [ "$API_FAILS" -eq 1 ]; then
      warn "API has no alive proxy; keeping last pin $CURRENT_SOCKS (fail-closed, no direct fallback)."
    elif [ $((API_FAILS % PROOF_HEARTBEAT_N)) -eq 0 ]; then
      warn "API still has no alive proxy; keeping last pin $CURRENT_SOCKS — $API_FAILS in a row."
    fi
    write_status "$CURRENT_SOCKS" "" "" false "api returned no alive proxy"
    continue
  fi
  API_FAILS=0
  socks="${fastest%%|*}"; pname="${fastest#*|}"
  if [ "$socks" != "$CURRENT_SOCKS" ]; then
    log "Upstream changed $CURRENT_SOCKS -> $socks ($pname), re-pinning..."
    PROOF_FAILS=0
    TUN_FAILS=0
    if start_tunnel "$socks" "$pname"; then
      if eip="$(verify_egress "$socks")"; then
        report_verified "$socks" "$pname" "$eip"
        write_status "$socks" "$pname" "$eip" true ""
      else
        warn "Egress proof failed after re-pin to $socks."
        write_status "$socks" "$pname" "" false "egress proof failed after re-pin"
      fi
    else
      warn "Re-pin to $socks failed."
      write_status "$socks" "$pname" "" false "re-pin failed"
    fi
    continue
  fi
  if ! tunnel_alive; then
    TUN_FAILS=$((TUN_FAILS + 1))
    if [ "$TUN_FAILS" -eq 1 ]; then
      warn "Tunnel device/process dead, restarting on $CURRENT_SOCKS ..."
    elif [ $((TUN_FAILS % PROOF_HEARTBEAT_N)) -eq 0 ]; then
      warn "Tunnel still dead, restarting on $CURRENT_SOCKS — $TUN_FAILS in a row."
    fi
    start_tunnel "$socks" "$pname" quiet || { write_status "$socks" "$pname" "" false "tunnel restart failed"; continue; }
  else
    TUN_FAILS=0
  fi
  if eip="$(verify_egress "$socks" quiet)"; then
    report_verified "$socks" "$pname" "$eip"
    write_status "$socks" "$pname" "$eip" true ""
  else
    rc=$?
    PROOF_FAILS=$((PROOF_FAILS + 1))
    case $rc in
      2) reason="MISMATCH" ;;
      3) reason="UNPROVEN" ;;
      *) reason="socks-down" ;;
    esac
    if [ "$PROOF_FAILS" -eq 1 ]; then
      warn "Egress proof failing via $socks ($pname): $reason — retrying, heartbeat every ${PROOF_HEARTBEAT_N}th while down."
    elif [ $((PROOF_FAILS % PROOF_HEARTBEAT_N)) -eq 0 ]; then
      warn "Egress proof still failing via $socks ($pname): $reason — $PROOF_FAILS in a row."
    fi
    write_status "$socks" "$pname" "" false "periodic egress proof failed"
  fi
done
