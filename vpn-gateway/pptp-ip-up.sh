#!/bin/sh
# pppd ip-up hook for PPTP sessions ONLY (wired via ip-up-script in
# options.pptpd - L2TP sessions keep the pppd default path, untouched).
# Args from pppd: $1=iface $2=tty $3=speed $4=local-IP $5=remote-IP $6=ipparam.
# Purely observational: logs one line per session so reconnect failures are
# diagnosable from a single file. Must never fail the connection.
LOG=/var/log/ppp-pptp.log
TS="$(date -u +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date)"
printf '%s [pptp-ip-up] iface=%s local=%s remote=%s\n' \
  "$TS" "${1:-?}" "${4:-?}" "${5:-?}" >> "$LOG" 2>/dev/null
exit 0
