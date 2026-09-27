#!/usr/bin/env bash
# cleanip-scan.sh — L2 escape hatch helper: find a reachable CDN edge IP.
#
# When the censor blocks specific Cloudflare/edge IPs (TCP RST / timeout on
# 443) while the SNI itself is still fine, dial the SAME SNI through a
# different anycast edge IP. Run this from the VM on the censored network,
# then set the winner in CLEAN_IP_MAP (v2proxy swaps the dial address at
# ingest; SNI/Host/UUID untouched):
#
#   ./scripts/cleanip-scan.sh cdn.example.com 443
#   # CLEAN_IP_MAP="blocked.edge=104.17.42.7"  <- paste suggestion into .env
#
# Usage: cleanip-scan.sh <sni-host> [port=443] [flags]
#   --for H     map key when subscription links dial a host/IP that differs
#               from the SNI (defaults to <sni-host>).
#   --via URL   proxy for fetching candidate lists when DIRECT is blocked,
#               e.g. --via socks5h://127.0.0.1:27017 (our own aggregate SOCKS)
#               or --via http://127.0.0.1:27016. Your working proxy bootstraps
#               the discovery of replacement IPs: direct is always tried first,
#               the proxy is fallback per-fetch, never required.
#   --samples N random IPs sampled per Cloudflare CIDR (default 6).
# Env: CLEAN_IP_CANDIDATES="ip1,ip2,..." replaces all discovery (you know best).
#      DOH_BASES="https://my.doh/dns-query ..." overrides the DoH resolvers
#        (bases must accept ?name=&type=; use when public DoH is blocked).
#      TCP_TIMEOUT=3 PROBE_JOBS=12 MAXCANDS=220 tune the sweep.
# Deps: bash, curl, timeout (coreutils), openssl, awk. python3 enables layer 2
# (soft — skipped with a notice when absent). No root, no traffic beyond short
# TCP/TLS handshakes plus 3 tiny HTTPS fetches (DoH x2, ips-v4 x1).
set -u

SNI="${1:-}"; PORT="${2:-443}"
FOR=""; VIA=""; SAMPLES=6
while [[ $# -gt 0 ]]; do case "$1" in
  --for) FOR="${2:-}"; shift 2;;
  --via) VIA="${2:-}"; shift 2;;
  --samples) SAMPLES="${2:-6}"; shift 2;;
  *) shift;;
esac done
[[ -z "$SNI" ]] && { echo "usage: $0 <sni-host> [port] [--for H] [--via PROXY] [--samples N]"; exit 2; }
[[ -z "$FOR" ]] && FOR="$SNI"
TCP_TIMEOUT="${TCP_TIMEOUT:-3}"; PROBE_JOBS="${PROBE_JOBS:-12}"; MAXCANDS="${MAXCANDS:-220}"

# get <url> [curl-args...]: direct first, --via proxy when direct is blocked.
get() {
  local url=$1; shift
  if curl -sS --max-time 10 "$url" "$@" 2>/dev/null; then return 0; fi
  if [[ -n "$VIA" ]]; then
    echo "  (direct blocked, retrying via proxy)" >&2
    curl -sS --max-time 15 --proxy "$VIA" "$url" "$@" 2>/dev/null
  else
    return 1
  fi
}

# Match "data":"1.2.3.4" with any spacing (compact CF/Google JSON has none,
# pretty-printing DoH servers do — the value regex stays strict IPv4 so AAAA
# records and CNAME hostnames can never leak in as candidates).
IPRE='"data"[[:space:]]*:[[:space:]]*"[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+"'

layer_doh() { # the domain's REAL current edge IPs — highest-value candidates
  local got="" body base
  for base in ${DOH_BASES:-https://cloudflare-dns.com/dns-query https://dns.google/resolve}; do
    body=$(get "$base?name=$SNI&type=A" -H 'accept: application/dns-json') || continue
    got+=" $(grep -oE "$IPRE" <<<"$body" | grep -oE '[0-9.]+' || true)"
  done
  echo "$got"
}

layer_ranges() { # official CF ranges, freshly sampled — never goes stale
  if ! command -v python3 >/dev/null 2>&1; then
    echo "  (python3 missing — skipping range sampling)" >&2; return
  fi
  local cidrs
  cidrs=$(get https://www.cloudflare.com/ips-v4) || {
    echo "  (ips-v4 unreachable, direct and via proxy)" >&2; return; }
  python3 -c '
import ipaddress, random, sys
n = int(sys.argv[1]); out = []
for line in sys.argv[2].split():
    try: net = ipaddress.ip_network(line.strip())
    except ValueError: continue
    if net.version != 4 or net.num_addresses < 4: continue
    hosts = net.num_addresses - 2
    for _ in range(min(n, hosts)):
        out.append(str(net.network_address + random.randint(1, hosts)))
print("\n".join(out))' "$SAMPLES" "$cidrs" 2>/dev/null || true
}

# Offline last resort: anycast means each of these serves any SNI on 443.
BUILTIN="104.16.0.0 104.17.32.32 104.18.64.64 104.19.128.128 104.20.64.64
104.21.96.96 104.24.1.1 104.27.1.1 172.64.32.32 172.67.1.1 141.101.115.0 188.114.97.7"

if [[ -n "${CLEAN_IP_CANDIDATES:-}" ]]; then
  CANDS=$(tr ',;' ' ' <<<"$CLEAN_IP_CANDIDATES")
  echo "[0/2] manual list: $(wc -w <<<"$CANDS") candidates (discovery skipped)" >&2
else
  echo "[0/2] gathering candidates..." >&2
  DOH=$(layer_doh); RANGES=$(layer_ranges)
  echo "  doh=$(wc -w <<<"$DOH") ranges=$(wc -w <<<"$RANGES") builtin=$(wc -w <<<"$BUILTIN")" >&2
  # shellcheck disable=SC2086
  CANDS=$(printf '%s\n' $DOH $RANGES $BUILTIN | awk 'NF && !seen[$0]++' | head -n "$MAXCANDS")
fi

probe_tcp() { # $1=ip -> prints "ms ip" on TCP success
  local ip=$1 start end
  start=$(date +%s%3N)
  if timeout "$TCP_TIMEOUT" bash -c "</dev/tcp/$ip/$PORT" 2>/dev/null; then
    end=$(date +%s%3N)
    echo "$((end - start)) $ip"
  fi
}
export -f probe_tcp; export PORT TCP_TIMEOUT

echo "[1/2] TCP sweep (${PORT}) over $(wc -w <<<"$CANDS") candidates..." >&2
sweep_once() {
  # shellcheck disable=SC2086
  xargs -P"$PROBE_JOBS" -n1 bash -c 'probe_tcp "$@"' _ $CANDS 2>/dev/null | sort -n | head -8
}
mapfile -t OPEN < <(sweep_once)
if [[ ${#OPEN[@]} -eq 0 ]]; then
  # DPI often blackholes a FRACTION of SYNs; a lone empty pass is not proof
  # of a dead network. One retry costs seconds when hosts are up (fast
  # answers) and ~one timeout round when truly blackholed.
  echo "  (first pass empty — retrying once for flap)" >&2
  sleep 2
  mapfile -t OPEN < <(sweep_once)
fi
[[ ${#OPEN[@]} -eq 0 ]] && { echo "no candidate answered TCP/${PORT} — L3-style blackout likely (see AGENT.md: IR-VPS relay)"; exit 1; }

echo "[2/2] TLS verify with SNI=${SNI} (top ${#OPEN[@]})..." >&2
WINNERS=()
for row in "${OPEN[@]}"; do
  ms=${row%% *}; ip=${row##* }
  if timeout 6 openssl s_client -connect "$ip:$PORT" -servername "$SNI" \
      -brief </dev/null >/dev/null 2>&1; then
    echo "  OK  ${ms}ms  $ip"
    WINNERS+=("$ms $ip")
  else
    echo "  tls-fail  $ip (TCP open, handshake killed — DPI filtering this edge/SNI)" >&2
  fi
done
[[ ${#WINNERS[@]} -eq 0 ]] && { echo "TCP opens but every TLS handshake dies — SNI itself is filtered; clean-IP cannot help (see AGENT.md: relay tiers)"; exit 1; }

echo
echo "Top clean IPs for SNI=$SNI:"
printf '  %s\n' "${WINNERS[@]:0:3}"
BEST=${WINNERS[0]##* }
echo
echo "Paste into ~/V2ProDock/.env, then: docker compose up -d --build v2proxy"
echo "  CLEAN_IP_MAP=\"$FOR=$BEST\""
