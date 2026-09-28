#!/bin/sh
# pppd ip-up hook for PPTP sessions ONLY (wired via ip-up-script in
# options.pptpd - L2TP sessions keep the pppd default path, untouched).
# Args from pppd: $1=iface $2=tty $3=speed $4=local-IP $5=remote-IP $6=ipparam.
# Purely observational: logs one line per session so reconnect failures are
# diagnosable from a single file. Must never fail the connection.
LOG=/var/log/ppp-pptp.log
# Same local timestamp + tag as the gateway logs so session lines correlate
# 1:1 with `docker logs v2prodock-vpn` (UTC ISO here used to split every
# timeline in two).
TS="$(date '+%Y-%m-%d %H:%M:%S' 2>/dev/null || date)"
printf '%s [v2prodock-vpn] [pptp-ip-up] iface=%s local=%s remote=%s\n' \
  "$TS" "${1:-?}" "${4:-?}" "${5:-?}" >> "$LOG" 2>/dev/null
exit 0
