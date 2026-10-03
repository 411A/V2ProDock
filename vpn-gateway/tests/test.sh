#!/bin/sh
# Strict static self-tests for vpn-gateway. No docker needed.
# Fails fast; used by CI and by install.sh --check.
# Lives in tests/; hop to the gateway root so all relative paths below work.
set -eu
cd "$(dirname "$0")/.."

fail=0
ok() { echo "[vpn-test][OK] $*"; }
bad() { echo "[vpn-test][FAIL] $*" >&2; fail=1; }

# 1. shell syntax
for f in entrypoint.sh watchdog.sh gen-mobileconfig.sh; do
  if sh -n "$f"; then ok "sh -n $f"; else bad "sh -n $f"; fi
done
command -v shellcheck >/dev/null 2>&1 && {
  if shellcheck -S warning entrypoint.sh watchdog.sh gen-mobileconfig.sh; then ok "shellcheck"; else bad "shellcheck"; fi
} || echo "[vpn-test][SKIP] shellcheck not installed"

# 2. template placeholders (entrypoint must replace all three)
for ph in __VPN_DOMAIN__ __VPN_SUBNET__ __VPN_DNS__; do
  grep -q "$ph" swanctl.conf.tmpl && ok "tmpl has $ph" || bad "tmpl missing $ph"
done
grep -q "__EAP_USERS__" swanctl.conf.tmpl && ok "tmpl has EAP marker" || bad "tmpl missing EAP marker"
grep -q "__IPSEC_PSK__" swanctl.conf.tmpl && ok "tmpl has PSK marker" || bad "tmpl missing PSK marker"
grep -q "eap-user" entrypoint.sh && ok "entrypoint renders eap subsections" || bad "entrypoint must render eap subsections"
grep -q 'id = ' entrypoint.sh && grep -q 'secret = ' entrypoint.sh && ok "secrets use id/secret form" || bad "secrets must use swanctl id/secret subsections"
if grep -Eq 'eap-[^ ]+ = "' entrypoint.sh; then bad "bare eap-X = string is invalid swanctl"; else ok "no invalid eap one-liners"; fi
grep -q "ike-l2tp" entrypoint.sh swanctl.conf.tmpl && ok "L2TP PSK secret wired" || bad "ike-l2tp PSK missing"
grep -q "swanctl --load-all" entrypoint.sh && ok "entrypoint loads swanctl" || bad "swanctl load missing"
grep -q "ipsec start" entrypoint.sh && ok "entrypoint starts via ipsec starter" || bad "must use ipsec starter (charon not on PATH)"
if grep -Eq '^[[:space:]]*charon([[:space:]]|$)' entrypoint.sh; then bad "bare charon invocation (not on PATH in Alpine)"; else ok "no bare charon call"; fi
grep -q "ikev2-eap" entrypoint.sh swanctl.conf.tmpl && ok "ikev2-eap conn wired" || bad "ikev2-eap missing"
grep -q "l2tp-psk" entrypoint.sh swanctl.conf.tmpl && ok "l2tp-psk conn wired" || bad "l2tp-psk missing"
[ "$(grep -c 'encap = yes' swanctl.conf.tmpl)" = "2" ] && ok "forced NAT-T encap on both conns (same-LAN ESP)" || bad "both conns need encap = yes"
[ "$(grep -c 'rekey_time = 1800s' swanctl.conf.tmpl)" = "2" ] && ok "child rekey 30min bounds silent desync" || bad "both children need rekey_time = 1800s"
grep -q 'swanctl --list-conns > /tmp/swanctl-conns.log' entrypoint.sh && ok "conn list saved before grep" || bad "must save list-conns to file before grepping"
if grep -Eq 'tee /tmp/swanctl-conns.log \| grep' entrypoint.sh; then bad "tee|grep -q SIGPIPE-truncates the file"; else ok "no SIGPIPE-truncated check"; fi
if grep -v '^[[:space:]]*#' entrypoint.sh watchdog.sh gen-mobileconfig.sh | grep -F '%.*.'; then bad "mid-pattern dot-star suffix derivation (posix-divergent)"; else ok "no fragile suffix derivations"; fi
grep -q "version = 1" swanctl.conf.tmpl && ok "IKEv1 for L2TP" || bad "l2tp conn must be version = 1"
grep -q "mode = transport" swanctl.conf.tmpl && ok "transport mode for L2TP" || bad "l2tp child must be transport mode"
grep -q "aes128-sha1," swanctl.conf.tmpl && ok "non-DH QM variants (legacy QM has no KE retry)" || bad "l2tp esp must offer non-DH transforms"
grep -q "1701" swanctl.conf.tmpl && ok "TS covers UDP 1701" || bad "child TS must cover udp/1701"

# 3. FAIL-CLOSED routing: BOTH VPN subnets must only leave via tun0
grep -q 'enforce_net "\$VPN_SUBNET"' entrypoint.sh && grep -q 'enforce_net "\$VPN_L2TP_NET"' entrypoint.sh \
  && ok "both pools enforced" || bad "enforce_net must cover IKEv2 + L2TP pools"
grep -q 'FORWARD -s "\$_net" -o "\$TUN_DEV" -j ACCEPT' entrypoint.sh && ok "forward VPN->tun0" || bad "forward rule missing"
grep -q 'FORWARD -s "\$_net" -j DROP' entrypoint.sh && ok "fail-closed DROP present" || bad "missing DROP for direct leaks"
if grep -Eq 'iptables -t nat -(A|C) POSTROUTING' entrypoint.sh; then bad "MASQUERADE breaks hev return path (replies die in INPUT)"; else ok "no MASQUERADE on VPN nets"; fi
grep -q 'FORWARD -i "\$TUN_DEV" -j ACCEPT' entrypoint.sh && ok "tun0 return path accepted (hev re-injects)" || bad "return path from tun0 must be ACCEPTed"
if grep -q 'FORWARD -i "\$TUN_DEV" -m state' entrypoint.sh; then bad "state match on tun0 blackholes hev replies (no conntrack)"; else ok "no conntrack match on tun0"; fi
grep -q 'iptables -t nat -D POSTROUTING' entrypoint.sh && ok "stale MASQ rules scrubbed" || bad "must scrub MASQ left by older builds"
if grep -q 'POSTROUTING.*-o eth0' entrypoint.sh; then bad "must never MASQUERADE VPN via eth0"; else ok "no eth0 masquerade"; fi
grep -q 'table 100' watchdog.sh entrypoint.sh && ok "policy routing table 100" || bad "table 100 routing missing"
grep -q 'from "\$VPN_NET" table 100' watchdog.sh && ok "source-based rule (no loop)" || bad "source-based rule missing"
grep -q 'from "\$VPN_L2TP_NET" table 100' watchdog.sh && ok "l2tp source-based rule" || bad "L2TP net missing table-100 rule"
grep -q 'from "\$VPN_PPTP_NET" table 100' watchdog.sh && ok "pptp source-based rule" || bad "PPTP net missing table-100 rule"
# The proof curls --interface tun0 (source TUN_ADDR): without a from-rule
# for that source the fib lookup rejects tun0-bound sockets (ENETUNREACH)
# and egress proof NEVER verifies, while VPN clients stay perfectly happy.
grep -q 'from "\$TUN_ADDR" table 100' watchdog.sh && ok "proof source (TUN_ADDR) routed via tun0" || bad "start_tunnel must add from-TUN_ADDR table100 rule - proof can never verify without it"
for _k in POLL_SECS PROOF_HEARTBEAT_N EGRESS_URL; do
  grep -q "${_k}=\${${_k}" ../docker-compose.yml && ok "compose passes $_k to gateway" || bad "documented knob $_k never reaches the gateway container (.env setting is dead)"
done
if grep -q 'table 100 pref 220' watchdog.sh entrypoint.sh; then bad "pref 220 collides with Docker per-network rules (PPTP would miss tun0)"; else ok "no pref-220 collision with Docker"; fi
grep -q 'for _pool in "\$VPN_SUBNET" "\$VPN_L2TP_NET" "\$VPN_PPTP_NET"' entrypoint.sh \
  && grep -q 'ip rule add to "\$_pool" lookup main pref 217' entrypoint.sh \
  && ok "to-pool exceptions precede from-rules" || bad "to-pool main-table exceptions (pref 217) missing"

# 4. Upstream pinning + egress proof (the core requirement)
grep -q 'V2PRODOCK_API' watchdog.sh && ok "watchdog polls v2prodock API" || bad "API poll missing"
grep -q '/proxies' watchdog.sh && ok "uses /proxies fastest" || bad "must use /proxies"
grep -q 'verify_egress' watchdog.sh && ok "egress proof fn present" || bad "verify_egress missing"
grep -q 'socks5-hostname' watchdog.sh && ok "probes via SOCKS" || bad "socks probe missing"
grep -q '\-\-interface.*TUN_DEV' watchdog.sh && ok "probes via tun0" || bad "tun probe missing"
grep -q 'VERIFIED' watchdog.sh && ok "VERIFIED proof logging" || bad "VERIFIED log missing"
grep -q 'EGRESS UNPROVEN' watchdog.sh && ok "tun-empty distinguished from true mismatch" || bad "must separate UNPROVEN (stalled tunnel) from MISMATCH (misrouting)"
grep -q 'LAST_VERIFIED' watchdog.sh && ok "VERIFIED logged on change only (no per-poll spam)" || bad "healthy polls must not log VERIFIED every 15s"
grep -q 'PROOF_FAILS' watchdog.sh && ok "consecutive proof-failure counter" || bad "periodic failures must carry a streak count"
grep -q 'PROOF_HEARTBEAT_N' watchdog.sh && ok "failure heartbeat (transition + every-Nth, no per-cycle spam)" || bad "steady-state failures must not log every poll"
# A pin can stay "alive" per the API while breaking the proof URL or tun
# path - rotation is the only escape from a live-but-unusable pin.
grep -q 'PROOF_ROTATE_FAILS' watchdog.sh && grep -q 'CURRENT_SOCKS=""' watchdog.sh \
  && ok "sustained proof failure drops the pin (re-pin/rebuild)" || bad "proof-failure rotation missing - a live-but-unusable pin sticks forever"
grep -q 'quiet' watchdog.sh && ok "quiet periodic verify mode" || bad "hot periodic path must not warn per cycle"
grep -q 'TUN_FAILS' watchdog.sh && ok "tunnel-restart spam throttled (transition + heartbeat)" || bad "crash-loop restarts must not log per poll"
grep -q '%H:%M:%S' watchdog.sh && grep -q '%H:%M:%S' entrypoint.sh && ok "timestamps on gateway logs" || bad "every gateway log line needs a timestamp"
grep -q 'status.json' watchdog.sh && ok "status.json for /vpn" || bad "status.json missing"
grep -q 'ensure_ipsec' watchdog.sh && grep -q 'swanctl --load-all' watchdog.sh \
  && ok "watchdog self-heals ipsec state" || bad "watchdog must reload conns if charon restarts"

# 5. L2TP daemon configs
grep -q '__L2TP_RANGE__' xl2tpd.conf.tmpl && grep -q '__L2TP_LOCAL__' xl2tpd.conf.tmpl \
  && ok "xl2tpd tmpl placeholders" || bad "xl2tpd tmpl missing placeholders"
grep -q 'pppoptfile' xl2tpd.conf.tmpl && ok "xl2tpd uses pppoptfile" || bad "xl2tpd must reference options.xl2tpd"
grep -q '# __MS_DNS__' options.xl2tpd.tmpl && ok "ppp dns marker" || bad "options tmpl missing MS_DNS marker"
grep -q 'chap-secrets' entrypoint.sh xl2tpd.conf.tmpl && ok "chap-secrets wired" || bad "chap-secrets missing"
grep -q 'chmod 600 /etc/ppp/chap-secrets' entrypoint.sh && ok "chap-secrets locked down" || bad "chap-secrets must be 600"
grep -q 'xl2tpd -D' entrypoint.sh && ok "xl2tpd supervised" || bad "entrypoint must start xl2tpd -D"
grep -q 'rm -f /var/run/xl2tpd.pid' entrypoint.sh && ok "stale xl2tpd pidfile cleared (docker restart safe)" || bad "must clear stale xl2tpd pidfiles"
grep -q ':1701 ' entrypoint.sh && ok "1701 listener verified at boot" || bad "boot must verify UDP 1701 listener"
grep -q 'VPN_IPSEC_PSK' entrypoint.sh && ok "PSK required at boot" || bad "VPN_IPSEC_PSK must be mandatory"
grep -q 'VPN_ALLOW_PLAIN_L2TP' entrypoint.sh && ok "plain-L2TP flag parsed" || bad "VPN_ALLOW_PLAIN_L2TP missing"
grep -q 'pol none -j DROP' entrypoint.sh && ok "bare L2TP dropped by default (xt_policy)" || bad "default must refuse bare L2TP at packet level"
grep -q 'CLEARTEXT' entrypoint.sh && ok "cleartext warning logged" || bad "plain mode must warn loudly"
grep -q 'plain_l2tp' watchdog.sh && ok "status exposes plain_l2tp" || bad "status.json must expose plain_l2tp"
grep -q 'VPN_ALLOW_PLAIN_L2TP' ../docker-compose.yml && ok "compose passes plain flag" || bad "compose must pass VPN_ALLOW_PLAIN_L2TP"
grep -q 'CA:TRUE' entrypoint.sh && ok "CA carries CA:TRUE (trust anchor)" || bad "CA must have CA:TRUE or clients reject it"
grep -q 'openssl verify -CAfile' entrypoint.sh && ok "cert chain verified at boot" || bad "boot must verify server cert chain"
grep -q 'san_entry' entrypoint.sh && ok "SAN typed IP:/DNS: (identity binding)" || bad "server SAN must type IPs as IP: and names as DNS:"
grep -q 'authorities' swanctl.conf.tmpl && grep -q 'cacert = ca.crt' swanctl.conf.tmpl \
  && ok "authorities section pins CA" || bad "swanctl needs authorities { vpn-ca { cacert } }"
if grep -F '*.*.*.*|*[!0-9' entrypoint.sh; then bad "inverted IP validation (dies on valid input)"; else ok "IP validation polarity correct"; fi
grep -q 'pfx3()' entrypoint.sh && ok "pool prefix consistency check present" || bad "L2TP pool must be consistency-checked"
grep -q '1701:1701/udp' ../docker-compose.yml && ok "compose maps UDP 1701" || bad "compose must map 1701/udp"
grep -q 'mknod /dev/ppp' entrypoint.sh && ok "/dev/ppp ensured at boot" || bad "boot must ensure /dev/ppp for pppd"
grep -q ': > /dev/ppp' entrypoint.sh && ok "/dev/ppp open-tested (existence is not enough)" || bad "boot must open-test /dev/ppp"
grep -q 'MKNOD' ../docker-compose.yml && ok "compose grants MKNOD" || bad "compose must grant MKNOD for /dev/ppp"
grep -q 'SYS_ADMIN' ../docker-compose.yml && ok "compose grants SYS_ADMIN (pppd line discipline)" || bad "compose must grant SYS_ADMIN"
grep -q "c 108:0 rwm" ../docker-compose.yml && ok "compose allows ppp device" || bad "compose needs device_cgroup_rules c 108:0 rwm"

# 6. Dockerfile sanity
grep -q 'strongswan' Dockerfile && ok "Dockerfile has strongswan" || bad "strongswan missing"
grep -q 'xl2tpd' Dockerfile && grep -q 'ppp' Dockerfile && ok "Dockerfile has xl2tpd+ppp" || bad "xl2tpd/ppp missing"
grep -q 'command -v xl2tpd' Dockerfile && ok "Dockerfile verifies daemons" || bad "daemon presence check missing"
grep -q 'heiher/hev-socks5-tunnel' Dockerfile && ok "Dockerfile has tun2socks (correct repo)" || bad "tun2socks repo wrong (must be heiher/hev-socks5-tunnel)"
if grep -q '|| true' Dockerfile; then bad "Dockerfile masks failures with || true"; else ok "no failure masking in Dockerfile"; fi
grep -q 'test -s /usr/local/bin/hev-socks5-tunnel' Dockerfile && ok "Dockerfile verifies tunnel binary" || bad "tunnel binary verification missing"
grep -q '500/udp' Dockerfile && grep -q '4500/udp' Dockerfile && ok "IKE ports exposed" || bad "IKE ports missing"
grep -q 'ENTRYPOINT' Dockerfile && ok "entrypoint set" || bad "ENTRYPOINT missing"
# 6b. PPTP (poptop) build + wiring
grep -q 'pptpd-1.4.0' Dockerfile && ok "Dockerfile builds poptop pptpd" || bad "pptpd source build missing"
grep -q 'make pptpd pptpctrl' Dockerfile && ok "pptpd builds only portable targets (no musl-broken bcrelay)" || bad "must build pptpd+pptpctrl only (bcrelay fails on musl)"
grep -q 'install -m755 pptpd /usr/local/sbin/pptpd' Dockerfile && ok "pptpd installed" || bad "pptpd install missing"
grep -q 'apk del .pptpd-build' Dockerfile && ok "build deps purged (small image)" || bad "must purge .pptpd-build deps"
grep -q '1723/tcp' Dockerfile && ok "PPTP port exposed" || bad "1723/tcp expose missing"
grep -q 'pptpd.conf.tmpl' Dockerfile && grep -q 'options.pptpd.tmpl' Dockerfile && ok "pptp templates copied" || bad "pptpd/options.pptpd COPY missing"
grep -q '__PPTP_LOCAL__' pptpd.conf.tmpl && grep -q '__PPTP_RANGE__' pptpd.conf.tmpl \
  && ok "pptpd tmpl placeholders" || bad "pptpd.conf.tmpl missing placeholders"
grep -q 'PPTP_SHORT_RANGE' entrypoint.sh && grep -q 'PPTP_END_LAST' entrypoint.sh \
  && ok "pptp range converted to poptop short form (full IP-IP exits 1)" || bad "must convert VPN_PPTP_RANGE to startIP-lastOctet (poptop cannot parse full IP-IP)"
if grep -q '^[[:space:]]*logwtmp' pptpd.conf.tmpl; then bad "logwtmp needs utmp/wtmp (absent in container)"; else ok "no logwtmp (no utmp in container)"; fi
grep -q 'option /etc/ppp/options.pptpd' pptpd.conf.tmpl && ok "pptpd uses options.pptpd" || bad "pptpd.conf must reference options.pptpd"
grep -q 'require-mppe-128' options.pptpd.tmpl && ok "pptp mandates MPPE-128" || bad "options.pptpd must require-mppe-128"
grep -q 'require-mschap-v2' options.pptpd.tmpl && ok "pptp mandates MSCHAPv2 (MPPE keys)" || bad "options.pptpd must require-mschap-v2"
if grep -Eq '^[[:space:]]*require (pap|chap|mschap)($|[[:space:]])' options.pptpd.tmpl; then bad "weak PPP methods must stay refused (MPPE needs MSCHAPv2)"; else ok "weak PPP methods refused"; fi
grep -q '# __MS_DNS__' options.pptpd.tmpl && ok "pptp dns marker" || bad "options.pptpd missing MS_DNS marker"
grep -q 'VPN_ENABLE_PPTP' entrypoint.sh && ok "pptp flag parsed" || bad "VPN_ENABLE_PPTP missing"
grep -q 'VPN_PPTP_NET' entrypoint.sh && ok "pptp pool validated" || bad "VPN_PPTP_NET validation missing"
grep -q 'enforce_net "\$VPN_PPTP_NET"' entrypoint.sh && ok "pptp pool fail-closed" || bad "enforce_net must cover PPTP pool"
grep -q 'INPUT -p tcp --dport 1723' entrypoint.sh && ok "1723 firewall rule" || bad "TCP 1723 INPUT rule missing"
grep -q 'INPUT -p gre -j ACCEPT' entrypoint.sh && ok "GRE firewall rule" || bad "GRE INPUT rule missing"
grep -q 'BROKEN' entrypoint.sh && ok "pptp breakage warning logged" || bad "must warn that PPTP crypto is broken"
grep -q 'pptpd -c /etc/pptpd/pptpd.conf -o /etc/ppp/options.pptpd -f' entrypoint.sh \
  && ok "pptpd supervised (foreground)" || bad "entrypoint must start pptpd -f with conf+options"
grep -q ':1723 ' entrypoint.sh && ok "1723 listener verified at boot" || bad "boot must verify TCP 1723 listener"
# 6c. PPTP reconnect resilience (zombies must die fast, redials must work)
grep -q 'lcp-echo-interval 10' options.pptpd.tmpl && grep -q 'lcp-echo-failure 3' options.pptpd.tmpl \
  && ok "pptp dead-peer detection ~30s (zombies free the redial)" || bad "LCP echo must be 10/3 for fast zombie reaping"
if grep -Eq '^[[:space:]]*lock([[:space:]]|$)' options.pptpd.tmpl; then bad "lock is pointless on per-call ptys (stale-lock failure mode)"; else ok "no ppp lock (per-call ptys)"; fi
if grep -Eq '^[[:space:]]*idle[[:space:]]' options.pptpd.tmpl; then bad "idle would hang up legitimately-idle satellite boxes"; else ok "no idle timeout (boxes idle 99%)"; fi
# 6d. Fast-handshake hygiene: never offer legacy peers what they only reject
for _opt in nopcomp noaccomp novj novjccomp noipv6; do
  grep -Eq "^[[:space:]]*${_opt}([[:space:]]|$)" options.pptpd.tmpl \
    && ok "pptp offers no ${_opt#no}" || bad "options.pptpd must set $_opt (legacy handshake hygiene)"
done
grep -Eq '^[[:space:]]*lcp-restart 2([[:space:]]|$)' options.pptpd.tmpl && ok "pptp LCP retransmit 2s" || bad "lcp-restart must be 2 (fast dial recovery)"
grep -Eq '^[[:space:]]*lcp-max-configure 10([[:space:]]|$)' options.pptpd.tmpl && ok "pptp LCP max-configure stated" || bad "lcp-max-configure 10 must be explicit"
grep -q 'ip-up-script /etc/ppp/pptp-ip-up' options.pptpd.tmpl && ok "pptp ip-up hook wired" || bad "options.pptpd must call pptp-ip-up"
grep -q 'ip-down-script /etc/ppp/pptp-ip-down' options.pptpd.tmpl && ok "pptp ip-down hook wired" || bad "options.pptpd must call pptp-ip-down"
grep -q 'conntrack-tools' Dockerfile && ok "Dockerfile has conntrack-tools" || bad "conntrack-tools missing (GRE flush needs it)"
grep -q 'pptp-ip-up' Dockerfile && grep -q 'pptp-ip-down' Dockerfile && ok "hooks installed by Dockerfile" || bad "Dockerfile must COPY both hooks"
grep -q 'conntrack --version' Dockerfile && ok "conntrack presence verified at build" || bad "build must verify conntrack binary"
grep -q 'pptp-ip-up' entrypoint.sh && grep -q 'pptp-ip-down' entrypoint.sh && ok "boot verifies hooks executable" || bad "entrypoint must fail fast on missing hooks"
grep -q '"$5"' pptp-ip-down.sh && ok "down-hook flushes by peer IP (\$5)" || bad "GRE flush must key on \$5 (peer IP)"
if grep -q -e '-s "$6"' pptp-ip-down.sh; then bad "must never flush by \$6 (ipparam, not an address)"; else ok "no \$6 flush bug"; fi
grep -q '^exit 0' pptp-ip-up.sh && grep -q '^exit 0' pptp-ip-down.sh && ok "hooks always exit 0 (never break PPP)" || bad "hooks must end with exit 0"
[ -f ../docker-compose.host.yml ] && ok "host-network override present" || bad "docker-compose.host.yml missing"
grep -q 'network_mode: host' ../docker-compose.host.yml && ok "host override uses host networking" || bad "host override must set network_mode: host"
grep -q '127.0.0.1:27018/proxies' ../docker-compose.host.yml && ok "host override points at host API" || bad "host override must use 127.0.0.1 API (no Docker DNS in host mode)"
grep -q 'sysctls: !reset' ../docker-compose.host.yml && ok "host override drops sysctls (runc forbids them in host netns)" || bad "host override must reset sysctls (ip_forward not allowed in host namespace)"
grep -q 'docker-compose.host.yml' ../README.md && ok "README documents host override" || bad "README must document docker-compose.host.yml"
grep -q 'experimental (alpha)' ../README.md && ok "README carries VPN alpha disclaimer" || bad "README must disclaim the VPN as experimental alpha"
if grep -q 'chap-secrets' pptpd.conf.tmpl options.pptpd.tmpl; then bad "pptp tmpls must not hardcode a secrets path (pppd default /etc/ppp/chap-secrets is the shared file)"; else ok "pptp uses default chap-secrets path (shared creds)"; fi
grep -q '"pptp":' watchdog.sh || grep -q "'pptp'" watchdog.sh || grep -q 'pptp' watchdog.sh && ok "status exposes pptp" || bad "status.json must expose pptp"
grep -q 'ensure_pptpd' watchdog.sh && ok "watchdog self-heals pptpd" || bad "watchdog must restart dead pptpd"
grep -q 'pkill -x pptpd' watchdog.sh && ok "pptpd stopped on shutdown" || bad "cleanup must stop pptpd"
grep -q '1723:1723/tcp' ../docker-compose.yml && ok "compose maps TCP 1723" || bad "compose must map 1723/tcp"
grep -q 'VPN_ENABLE_PPTP' ../docker-compose.yml && ok "compose passes pptp flag" || bad "compose must pass VPN_ENABLE_PPTP"
grep -q 'VPN_PPTP_NET' ../docker-compose.yml && ok "compose passes pptp net" || bad "compose must pass VPN_PPTP_NET"
grep -q 'VPN_ENABLE_PPTP' ../.env.example && ok ".env documents pptp flag" || bad ".env.example must document VPN_ENABLE_PPTP"
grep -q 'MPPE-128' ../.env.example || grep -q 'MPPE' ../.env.example && ok ".env warns about pptp crypto" || bad ".env.example must warn about PPTP crypto"

# 7. hev config keys
grep -q 'ipv4:' watchdog.sh && ok "hev ipv4 set" || bad "hev ipv4 missing"
grep -q 'udp: udp' watchdog.sh && ok "hev udp enabled (DNS/legacy)" || bad "hev udp missing"

# 7b. compose exposure + liveness (control plane must not face the LAN,
# health must prove serving, not just process existence).
grep -q '127.0.0.1:27018:27018' ../docker-compose.yml && ok "API published host-local only (/refresh is unauthenticated)" || bad "API must bind 127.0.0.1 (LAN-wide /refresh = self-DoS)"
if grep -Eq '"27018:27018"' ../docker-compose.yml; then bad "bare 27018 publish exposes control plane to LAN"; else ok "no LAN-wide API publish"; fi
grep -q '/health' ../docker-compose.yml && ok "compose healthcheck proves API liveness" || bad "healthcheck must curl /health, not kill -0"
grep -q 'start_period' ../docker-compose.yml && ok "healthcheck start_period covers thin-pool populates" || bad "healthcheck needs start_period (populate grinds for minutes)"
grep -q 'max-size' ../docker-compose.yml && ok "log rotation capped (disk-exhaustion safe)" || bad "compose logging needs max-size/max-file"
grep -q 'xray version' ../Dockerfile && ok "v2prodock build smoke-tests the xray binary" || bad "Dockerfile must run xray version after download"
grep -q -- '--retry' ../Dockerfile && ok "v2prodock download retries flaky builds" || bad "xray download needs --retry"
grep -q -- '--tries=3' Dockerfile && grep -q -- '--retry' Dockerfile && ok "vpn-gateway downloads retry" || bad "poptop/hev downloads need retry flags"

# 7c. fresh rebuild discipline (install.sh): every rerun tears THIS project
# down to zero (containers+orphans+network+images) before rebuilding, so no
# stale port-holder or dangling layer survives - without touching neighbors.
grep -q 'fresh_rebuild()' ../install.sh && ok "fresh_rebuild defined" || bad "install.sh needs fresh_rebuild()"
grep -q 'compose down --remove-orphans' ../install.sh && ok "rebuild removes orphans+network" || bad "fresh_rebuild must down --remove-orphans"
grep -q 'compose ps -aq' ../install.sh && ok "rebuild aborts on stuck survivors" || bad "fresh_rebuild must refuse to build over stuck containers"
for _img in $(grep -oE 'image: [^ ]+' ../docker-compose.yml | tr -d '\r' | awk '{print $2}'); do
  grep -q "docker rmi.*$_img" ../install.sh && ok "rebuild deletes image $_img" || bad "install.sh rmi names must mirror compose image: fields ($_img)"
done
grep -q 'address already in use' ../install.sh && ok "bind-conflict guidance present" || bad "up-failure path must explain squatter eviction"
# Egress failure must print the FACTS itself: "go run these commands" dies
# between paste and execution (container already gone) and "rerun the
# rebuild" was proven useless (fresh network + fresh containers, still
# ENETUNREACH - hydravm 2026-09-28).
grep -q 'netns_report()' ../install.sh && ok "egress failure prints netns facts" || bad "check_container_egress must dump netns facts itself (netns_report)"
grep -q 'has NO default route' ../install.sh && ok "default-route preflight before curl checks" || bad "check_container_egress must preflight the container default route"
if grep -q 'rerun install.sh (fresh_rebuild' ../install.sh; then bad "dead-end hint: fresh_rebuild proven NOT to fix ENETUNREACH"; else ok "no proven-useless rebuild hint"; fi
grep -q 'compose down --remove-orphans >/dev/null 2>&1' ../install.sh && ok "bind-retry starts from a clean slate" || bad "fresh_rebuild must down between bind retries (half-built endpoint must not be inherited)"
grep -q 'sudo -n modprobe' ../install.sh && ok "modprobe via passwordless sudo for non-root runs" || bad "ensure_host_prereqs must sudo -n modprobe when non-root"
grep -q 'modules-load.d/v2prodock' ../install.sh && ok "kernel modules persist across reboot" || bad "must write /etc/modules-load.d (PPTP GRE helpers vanish on reboot without it)"
grep -q 'sudo -n ufw' ../install.sh && ok "ufw opens applied via passwordless sudo" || bad "ufw auto-open must work without prompting (root or sudo -n)"
if grep -q 'stay blocked until you open' ../install.sh; then bad "ufw FUD: Docker-published ports BYPASS ufw - they were never blocked"; else ok "ufw message accurate (Docker bypasses it)"; fi
# The API port must never sit inside a published range: the daemon would
# program that host port twice (0.0.0.0 from the range + 127.0.0.1 for the
# API) - second bind EADDRINUSE on every pristine start, aborted start =
# half-programmed netns = container ENETUNREACH (proven: hydravm 4/4).
_api_port=$(grep -oE '127\.0\.0\.1:[0-9]+:[0-9]+' ../docker-compose.yml | tr -d '\r' | head -1 | cut -d: -f2)
if [ -z "$_api_port" ]; then
  bad "compose must publish the API on 127.0.0.1:<port>:<port>"
else
  _ov=0
  for _r in $(grep -oE '"[0-9]+-[0-9]+:[0-9]+-[0-9]+"' ../docker-compose.yml | tr -d '"\r'); do
    _h=${_r%%:*}; _lo=${_h%%-*}; _hi=${_h#*-}
    if [ "$_api_port" -ge "$_lo" ] && [ "$_api_port" -le "$_hi" ]; then _ov=1; fi
  done
  [ "$_ov" -eq 1 ] && bad "API port $_api_port is inside a published port range (dual-bind EADDRINUSE)" || ok "API port excluded from published ranges"
fi
# v2prodock runs BusyBox ip: '-br' does not exist there (prints usage), so
# the fact dump / verdict would read garbage. '-o' works on both.
if grep -q 'docker exec v2prodock ip -br' ../install.sh; then bad "netns_report uses ip -br (BusyBox v2prodock prints usage instead of addresses)"; else ok "netns fact dump uses BusyBox-safe ip -o"; fi

# 7c. log consistency: every line timestamped + tagged, levels uniform.
# (An untagged line in docker logs is indistinguishable noise.)
grep -q '^warn() ' watchdog.sh entrypoint.sh && ok "warn() with [WARN] tag in both scripts" || bad "warn() missing (warnings must carry the [WARN] tag)"
if grep -q 'log "WARN' entrypoint.sh watchdog.sh; then bad "WARN: smuggled inside info-level log() (use warn())"; else ok "no WARN: inside info log()"; fi
grep -q 'set -o pipefail' entrypoint.sh && ok "entrypoint pipefail set" || bad "entrypoint needs set -o pipefail like watchdog"
grep -q 'trap .rm -f /tmp/eap-secrets.conf' entrypoint.sh && ok "secret temp files trapped for die-paths" || bad "EAP/PSK temp files must be EXIT-trapped (failed boots leak creds in /tmp)"
grep -q 'chmod 600 "\$SWANCTL_CONF"' entrypoint.sh && ok "swanctl.conf 0600 (holds PSK+EAP secrets)" || bad "swanctl.conf must be chmod 600 after render"
grep -q 'render_ms_dns' entrypoint.sh && ok "all VPN_DNS servers rendered (no silent 3rd+ drop)" || bad "ms-dns must loop the full DNS list"
grep -q '^log "Wrote \$OUT' gen-mobileconfig.sh && ok "mobileconfig success line timestamped+tagged" || bad "gen-mobileconfig must log through log(), not bare echo"
if grep -Eq '^echo "Wrote ' gen-mobileconfig.sh; then bad "bare echo in gen-mobileconfig (log line without timestamp)"; else ok "no bare echo in gen-mobileconfig"; fi
grep -q "case \"\$POLL_SECS\"" watchdog.sh && ok "POLL_SECS validated (garbage must not CrashLoop the watchdog)" || bad "POLL_SECS needs a numeric guard"
grep -q 'local ts verified err' watchdog.sh && ok "write_status uses locals (no global leakage)" || bad "write_status must declare locals"
grep -q "%Y-%m-%d %H:%M:%S" pptp-ip-up.sh && grep -q "%Y-%m-%d %H:%M:%S" pptp-ip-down.sh \
  && grep -q 'v2prodock-vpn' pptp-ip-up.sh && grep -q 'v2prodock-vpn' pptp-ip-down.sh \
  && ok "ppp hooks share gateway timestamp+tag (single timeline)" || bad "ppp hooks must use the gateway timestamp format and tag"
if grep -q 'date -u' pptp-ip-up.sh pptp-ip-down.sh; then bad "ppp hooks on UTC while gateway is local (split timelines)"; else ok "no UTC/local timestamp split in hooks"; fi

# 7b. failure classification + sticky pinning (static)
grep -q 'set -o pipefail' watchdog.sh && ok "pipefail set (curl|tr must not mask failures)" || bad "watchdog.sh needs set -o pipefail (else tun timeouts misreport as empty)"
grep -q 'api_snapshot' watchdog.sh && ok "single API snapshot per poll" || bad "api_snapshot missing (one fetch per poll)"
grep -q 'api_current_alive' watchdog.sh && ok "sticky-pin liveness check present" || bad "api_current_alive missing (fastest-flap causes re-pin churn)"

# 8. Behavioral mocks: run the REAL extracted functions with a stubbed curl
# and a file:// API snapshot (bash required: only it and the image's busybox
# ash support set -o pipefail among the shells here).
if command -v bash >/dev/null 2>&1 && command -v curl >/dev/null 2>&1 && command -v jq >/dev/null 2>&1; then
  _bw="$(mktemp -d)"
  for _fn in verify_egress api_snapshot api_fastest api_current_alive api_name_for_port; do
    sed -n "/^${_fn}() {/,/^}/p" watchdog.sh >> "$_bw/fns.sh"
  done
  if [ -s "$_bw/fns.sh" ]; then ok "extracted live functions for mock run"; else bad "function extraction failed"; fi
  # Mock curl: socks leg and tun leg behave per env (ip|empty|fail).
  # file:// URLs pass through to the real curl (API snapshot tests).
  _realcurl="$(command -v curl)"
  cat > "$_bw/curl" <<EOF
#!/bin/sh
for a in "\$@"; do case "\$a" in file://*) exec "$_realcurl" "\$@" ;; esac; done
leg=""
for a in "\$@"; do
  case "\$a" in --socks5-hostname) leg="socks" ;; --interface) leg="tun" ;; esac
done
case "\$leg" in
  socks) mode="\$CURL_SOCKS" ;;
  tun) mode="\$CURL_TUN" ;;
  *) mode="fail" ;;
esac
case "\$mode" in
  ip) echo '9.9.9.9'; exit 0 ;;
  empty) exit 0 ;;
  fail|*) exit 28 ;;
esac
EOF
  chmod +x "$_bw/curl"
  cat > "$_bw/run.sh" <<'EOF'
# Sourced, never executed: stubs + the real extracted functions.
set -o pipefail
ts() { printf 'TS'; }
log() { echo "LOG $*"; }
warn() { echo "WARN $*" >&2; }
# shellcheck disable=SC1090
. "$FNS"
EOF
  _run() { PATH="$_bw:/usr/bin:/bin" FNS="$_bw/fns.sh" TUN_DEV=tun0 EGRESS_URL=http://example.invalid bash -c ". \"$_bw/run.sh\"; $1"; }
  # 8a. tun leg TIMES OUT (curl rc=28, silent): must say "no response", never "empty".
  _out="$(_run 'CURL_SOCKS=ip CURL_TUN=fail verify_egress "v2prodock:27028" loud 2>&1; echo "rc=$?"' )"
  case "$_out" in *"tun gave no response"*rc=3*) ok "tun timeout classified as no-response (pipefail works)" ;;
    *) bad "tun timeout misclassified: $_out" ;; esac
  # 8b. tun leg returns HTTP 200 with EMPTY body: the genuine "empty" case still works.
  _out="$(_run 'CURL_SOCKS=ip CURL_TUN=empty verify_egress "v2prodock:27028" loud 2>&1; echo "rc=$?"')"
  case "$_out" in *"empty tun response"*rc=3*) ok "true empty tun body still reported as empty" ;;
    *) bad "empty-body branch broken: $_out" ;; esac
  # 8c. both legs agree on an IP: proven, IP on stdout.
  _out="$(_run 'CURL_SOCKS=ip CURL_TUN=ip verify_egress "v2prodock:27028" loud 2>&1; echo "rc=$?"')"
  case "$_out" in *"9.9.9.9"*rc=0*) ok "matching legs prove egress" ;;
    *) bad "proof path broken: $_out" ;; esac
  # 8d. socks leg itself down: rc 1, distinct message.
  _out="$(_run 'CURL_SOCKS=fail CURL_TUN=ip verify_egress "v2prodock:27028" loud 2>&1; echo "rc=$?"')"
  case "$_out" in *"SOCKS itself down"*rc=1*) ok "dead socks leg classified as socks-down" ;;
    *) bad "socks-down branch broken: $_out" ;; esac
  # 8e. sticky pinning against a canned API snapshot (real curl, file://).
  printf '%s' '[{"socks5":"0.0.0.0:27021","name":"Alpha"},{"socks5":"0.0.0.0:27024","name":"Beta"}]' > "$_bw/api.json"
  _api() { PATH="/usr/bin:/bin" FNS="$_bw/fns.sh" V2PRODOCK_API="file://$_bw/api.json" API_SNAPSHOT="$_bw/snap.json" V2PRODOCK_SOCKS_HOST=v2prodock CURRENT_SOCKS="$2" bash -c ". \"$_bw/run.sh\"; $1"; }
  _out="$(_api 'api_snapshot && api_fastest' '')"
  [ "$_out" = "v2prodock:27021|Alpha" ] && ok "api_fastest parses snapshot" || bad "api_fastest wrong: $_out"
  _api 'api_snapshot >/dev/null && api_current_alive' 'v2prodock:27024' \
    && ok "current pin in alive set stays" || bad "api_current_alive missed a live pin"
  _api 'api_snapshot >/dev/null && api_current_alive' 'v2prodock:27099' \
    && bad "api_current_alive kept a dead pin" || ok "dead pin detected (re-pin allowed)"
  _out="$(_api 'api_snapshot >/dev/null && api_name_for_port 27024' '')"
  [ "$_out" = "Beta" ] && ok "sticky pin resolves its name" || bad "api_name_for_port wrong: $_out"
  # 8f. ms-dns renderer (entrypoint): all servers land, 3rd+ included.
  sed -n "/^render_ms_dns() {/,/^}/p" entrypoint.sh >> "$_bw/fns.sh"
  cat > "$_bw/dns.sh" <<'EOF'
set -o pipefail
die() { echo "DIE $*" >&2; exit 1; }
. "$FNS"
render_ms_dns "$DNSLIST" "$OUT"
cat "$OUT"
EOF
  _out="$(FNS="$_bw/fns.sh" DNSLIST="1.1.1.1,8.8.8.8,9.9.9.9" OUT="$_bw/ms.conf" bash "$_bw/dns.sh")"
  [ "$_out" = "$(printf 'ms-dns 1.1.1.1\nms-dns 8.8.8.8\nms-dns 9.9.9.9')" ] \
    && ok "all 3 DNS servers rendered (none dropped)" || bad "ms-dns render wrong: $_out"
  rm -rf "$_bw"
else
  echo "[vpn-test][SKIP] behavioral mocks need bash+curl+jq"
fi

# 10. free_port_squatter behavioral mocks (install.sh): the auto-evictor must
# kill a stale docker-proxy, and refuse a live one's proxy or any foreign
# process. All four OS tools are stubbed; only bash is required.
if command -v bash >/dev/null 2>&1; then
  _sq="$(mktemp -d)"
  # Suite cd's to vpn-gateway, so repo-root install.sh is ../install.sh.
  sed -n "/^free_port_squatter() {/,/^}/p" ../install.sh > "$_sq/fns.sh"
  if [ -s "$_sq/fns.sh" ]; then ok "extracted squatter fn for mock run"; else bad "squatter fn extraction failed"; fi
  mkdir -p "$_sq/bin"
  cat > "$_sq/bin/ss" <<'EOF'
#!/bin/sh
# SS_LINE canned per case; empty = port free.
printf '%s' "$SS_LINE"
EOF
  cat > "$_sq/bin/ps" <<'EOF'
#!/bin/sh
printf '%s' "$PS_ARGS"
EOF
  cat > "$_sq/bin/docker" <<'EOF'
#!/bin/sh
# docker inspect <cid> -> $INSPECT_RC (0 = container alive).
exit "${INSPECT_RC:-1}"
EOF
  chmod +x "$_sq/bin/"*
  cat > "$_sq/run.sh" <<'EOF'
RED=''; NC=''
# kill is a shell builtin: a PATH shim can never override it, so the mock
# is a function (overrides the builtin) logging every kill attempt.
kill() { printf '%s\n' "$*" >> "$KILL_LOG"; return 0; }
. "$FNS"
EOF
  _sqrun() { : > "$_sq/kill.log"; PATH="$_sq/bin:/usr/bin:/bin" FNS="$_sq/fns.sh" KILL_LOG="$_sq/kill.log" SS_LINE="$2" PS_ARGS="$3" INSPECT_RC="$4" bash -c ". \"$_sq/run.sh\"; free_port_squatter \"$1\" >/dev/null 2>&1; echo \"rc=\$?\""; }
  _stale_ss='LISTEN 0 4096 127.0.0.1:27018 0.0.0.0:* users:(("docker-proxy",pid=1234,fd=4))'
  _stale_ps='docker-proxy -proto tcp -host-ip 127.0.0.1 -host-port 27018 -container-ip 172.18.0.2 -container-port 27018 -container-id deadbeef1234'
  _out="$(_sqrun '127.0.0.1:27018/tcp' "$_stale_ss" "$_stale_ps" 1)"
  if [ "$_out" = "rc=0" ] && grep -q -- '-9 1234' "$_sq/kill.log"; then
    ok "stale proxy (dead container) killed by pid"
  else
    bad "stale proxy must die: $_out kill=$(cat "$_sq/kill.log")"
  fi
  _out="$(_sqrun '127.0.0.1:27018/tcp' "$_stale_ss" "$_stale_ps" 0)"
  if [ "$_out" = "rc=1" ] && [ ! -s "$_sq/kill.log" ]; then
    ok "live container proxy refused (no kill)"
  else
    bad "live proxy must be refused: $_out kill=$(cat "$_sq/kill.log")"
  fi
  _for_ss='LISTEN 0 4096 127.0.0.1:27018 0.0.0.0:* users:(("python3",pid=777,fd=5))'
  _out="$(_sqrun '127.0.0.1:27018/tcp' "$_for_ss" 'python3 app.py' 1)"
  if [ "$_out" = "rc=1" ] && [ ! -s "$_sq/kill.log" ]; then
    ok "foreign process refused (no kill)"
  else
    bad "foreign process must be refused: $_out kill=$(cat "$_sq/kill.log")"
  fi
  _out="$(_sqrun '127.0.0.1:27018/tcp' '' '' 1)"
  [ "$_out" = "rc=0" ] && ok "free port returns 0 silently" || bad "free port must return 0: $_out"
  # Static: the safety rails must exist around the kill.
  grep -q 'refusing to kill' ../install.sh && ok "refusal branches present" || bad "evictor must refuse live/foreign holders"
  grep -q 'command -v ss' ../install.sh && grep -q 'command -v ps' ../install.sh && ok "tool guards present" || bad "evictor must degrade without ss/ps"
  if grep -Eq '^[[:space:]]*(sudo[[:space:]]+)?systemctl (restart|stop|start) docker' ../install.sh; then
    bad "daemon restart must never run automatically (echo-only hammer)"
  else
    ok "no automatic daemon restart (neighbors safe)"
  fi
  rm -rf "$_sq"
else
  echo "[vpn-test][SKIP] squatter mocks need bash"
fi

# 11. verify_project_containers (install.sh): must report the real state of the
# containers compose owns, and do NOTHING else. Read-only is the whole safety
# argument — an earlier revision scanned every container on the host and
# removed strays by image pattern, which is a sharp instrument pointed at
# other people's stacks over a wrong diagnosis.
if command -v bash >/dev/null 2>&1; then
  _vc="$(mktemp -d)"
  sed -n "/^PROJECT_CONTAINERS=/p; /^verify_project_containers() {/,/^}/p" ../install.sh > "$_vc/fns.sh"
  [ -s "$_vc/fns.sh" ] && ok "extracted verify fn for mock run" || bad "verify fn extraction failed"
  mkdir -p "$_vc/bin"
  cat > "$_vc/bin/docker" <<'EOF'
#!/bin/sh
# docker inspect --format <tpl> <name> -> canned per-name state, logged.
case "${1:-}" in
    inspect)
        printf 'inspect %s\n' "$*" >> "$LOG"
        case "${4:-}" in
            v2prodock)     echo '/v2prodock|running|unless-stopped' ;;
            v2prodock-vpn) echo '/v2prodock-vpn|exited|no' ;;
            *) exit 1 ;;
        esac
        exit 0 ;;
    *) printf 'MUTATION %s\n' "$*" >> "$LOG"; exit 0 ;;
esac
EOF
  chmod +x "$_vc/bin/"*
  cat > "$_vc/run.sh" <<'EOF'
RED=''; NC=''
ok() { echo "OK $1"; }
err() { echo "ERR $1"; }
. /tmp/vc-fns-placeholder
EOF
  sed -i "s#/tmp/vc-fns-placeholder#$_vc/fns.sh#" "$_vc/run.sh"
  : > "$_vc/log"
  _vcout="$(PATH="$_vc/bin:/usr/bin:/bin" LOG="$_vc/log" bash -c ". \"$_vc/run.sh\"; verify_project_containers" 2>&1)"
  case "$_vcout" in
    *"v2prodock: running (restart=unless-stopped)"*) ok "running container reported with its policy" ;;
    *) bad "running state not reported: $_vcout" ;;
  esac
  case "$_vcout" in
    *"v2prodock-vpn: exited (restart=no)"*) ok "non-running container reported truthfully" ;;
    *) bad "exited state not reported: $_vcout" ;;
  esac
  case "$_vcout" in
    *"restart='no'"*) ok "missing restart policy flagged (no reboot survival)" ;;
    *) bad "restart policy gap must be flagged: $_vcout" ;;
  esac
  case "$_vcout" in
    *"not running"*) ok "non-running container flagged" ;;
    *) bad "non-running container must be flagged: $_vcout" ;;
  esac
  # The safety property: inspect only. Any create/rm/stop/update/kill would
  # land in the log as a MUTATION line.
  if grep -q MUTATION "$_vc/log"; then
    bad "verify must be read-only, it called: $(grep MUTATION "$_vc/log")"
  else
    ok "verify is read-only (docker inspect only)"
  fi
  [ "$(grep -c '^inspect ' "$_vc/log")" = "2" ] \
    && ok "inspects exactly the two compose containers" \
    || bad "verify must inspect only PROJECT_CONTAINERS, got: $(grep -c '^inspect ' "$_vc/log") calls"
  rm -rf "$_vc"
else
  echo "[vpn-test][SKIP] verify mocks need bash"
fi

# 12. Container identity contract (docker-compose.yml + install.sh). This
# project creates containers through compose and nowhere else, so the compose
# file IS the naming/restart guarantee — these guards keep it true. They catch
# the real regression (someone adds a service and forgets container_name /
# restart, and that container silently never returns after a reboot).
# CR-stripped copies first: this suite runs on Windows checkouts too, where a
# CRLF compose file makes every $ anchor silently stop matching.
cf=$(mktemp); tr -d '\r' < ../docker-compose.yml > "$cf"
is=$(mktemp); tr -d '\r' < ../install.sh > "$is"
sb=$(mktemp)   # only the services: block — a top-level `networks:` section has
               # the same 2-space shape as a service name and skews every count
awk '/^services:/{ins=1;next} /^[^[:space:]#]/{ins=0} ins' "$cf" > "$sb"
_svc=$(grep -cE '^  [a-z0-9_-]+:[[:space:]]*$' "$sb" || true)
_cn=$(grep -cE '^[[:space:]]*container_name:[[:space:]]*v2prodock' "$sb" || true)
_rs=$(grep -cE '^[[:space:]]*restart:[[:space:]]*unless-stopped[[:space:]]*$' "$sb" || true)
_im=$(grep -cE '^[[:space:]]*image:[[:space:]]*v2prodock/' "$sb" || true)
[ "$_svc" -gt 0 ] && ok "compose declares $_svc service(s)" || bad "no services found in compose"
[ "$_svc" = "$_cn" ] \
  && ok "every compose service pins a v2prodock* container_name ($_svc)" \
  || bad "every service needs container_name: v2prodock* (services=$_svc named=$_cn — a random name is unmanageable)"
[ "$_svc" = "$_rs" ] \
  && ok "every compose service sets restart: unless-stopped ($_svc)" \
  || bad "every service needs restart: unless-stopped (services=$_svc with-policy=$_rs — no policy = stays down through reboots)"
[ "$_svc" = "$_im" ] \
  && ok "every compose image is project-namespaced v2prodock/* ($_svc)" \
  || bad "every service image must start with v2prodock/ (services=$_svc namespaced=$_im)"
# install.sh's name list must not drift from the compose file, or `status`
# would report on containers that no longer exist.
_shn=$(sed -n 's/^PROJECT_CONTAINERS="\(.*\)"$/\1/p' "$is")
for n in $(sed -n 's/^[[:space:]]*container_name:[[:space:]]*//p' "$sb" | tr -d '"'); do
  case " $_shn " in
    *" $n "*) ok "install.sh knows container '$n'" ;;
    *) bad "install.sh PROJECT_CONTAINERS is missing '$n' (drift from compose)" ;;
  esac
done
# verify must stay read-only forever: it is the only code in this project that
# inspects containers it did not just create.
if grep -v '^[[:space:]]*#' "$is" | sed -n '/^verify_project_containers() {/,/^}/p' \
   | grep -Eq 'docker[[:space:]]+(rm|stop|kill|update|run|create|start|restart)\b'; then
  bad "verify_project_containers must only inspect — never mutate or remove"
else
  ok "verify_project_containers inspects only"
fi
grep -q 'verify_project_containers' "$is" \
  && ok "installer reports real container state" \
  || bad "install.sh must verify the containers it just started"
# POSIX sh: no process substitution. Slice the status branch out first.
_st=$(sed -n '/^        status)/,/^            ;;/p' "$is")
case "$_st" in
  *verify_project_containers*) ok "status reports container state" ;;
  *) bad "status must show container state" ;;
esac
# Docker has no `docker rename` for containers: an unnamed container can only
# be reported or removed, never adopted under a better name. Guard against a
# future "fix" that reaches for the impossible call.
grep -v '^[[:space:]]*#' "$is" | grep -Eq 'docker[[:space:]]+rename' \
  && bad "there is no 'docker rename' for containers — that call cannot work" \
  || ok "no fictional 'docker rename' call"
# Never touch containers outside this project, by any means.
if grep -v '^[[:space:]]*#' "$is" | grep -Eq 'docker[[:space:]]+(rm|kill)[[:space:]].*\$\(docker[[:space:]]+ps'; then
  bad "must never bulk-remove containers discovered by scanning the host"
else
  ok "no bulk removal by host scan"
fi
rm -f "$cf" "$is" "$sb"

if [ "$fail" -ne 0 ]; then echo "[vpn-test] FAILED" >&2; exit 1; fi
echo "[vpn-test] ALL PASS"
