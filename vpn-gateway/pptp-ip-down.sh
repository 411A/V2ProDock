#!/bin/sh
# pppd ip-down hook for PPTP sessions ONLY (see pptp-ip-up.sh for arg layout).
# Two jobs:
#  1. Log the teardown (timestamps + peer IP make reconnect failures
#     diagnosable: a redial that never logs ip-up died before PPP).
#  2. Best-effort purge of this peer's GRE conntrack entries so a fast
#     redial with a reused Call ID is not mistaken for the dead session.
#     Namespace caveat (read carefully): behind Docker bridge NAT the NAT
#     table lives in the HOST netns, which this container cannot see - there
#     this flush is a harmless no-op and host networking
#     (docker-compose.host.yml) is the real fix. Under network_mode: host
#     this flush DOES clear the stale Call-ID mapping.
# $5 is the peer (remote) IP. $6 is ipparam (an opaque string, NOT an
# address) - flushing by $6 would be wrong; this script uses $5 only.
# Never fails anything: every fallible step ends in `|| true`, exit 0.
LOG=/var/log/ppp-pptp.log
TS="$(date -u +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date)"
printf '%s [pptp-ip-down] iface=%s local=%s remote=%s\n' \
  "$TS" "${1:-?}" "${4:-?}" "${5:-?}" >> "$LOG" 2>/dev/null || true
if [ -n "${5:-}" ] && command -v conntrack >/dev/null 2>&1; then
  conntrack -D -p gre -s "$5" >> "$LOG" 2>&1 || true
fi
exit 0
