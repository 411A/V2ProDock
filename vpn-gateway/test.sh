#!/bin/sh
# Strict static self-tests for vpn-gateway. No docker needed.
# Fails fast; used by CI and by install.sh --check.
set -eu
cd "$(dirname "$0")"

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

if [ "$fail" -ne 0 ]; then echo "[vpn-test] FAILED" >&2; exit 1; fi
echo "[vpn-test] ALL PASS"
