#!/bin/bash
set -uo pipefail

RED='\033[0;31m'; GREEN='\033[0;32m'; CYAN='\033[0;36m'; NC='\033[0m'
ok() { echo -e "${GREEN}[OK] $1${NC}"; }
err() { echo -e "${RED}[ERROR] $1${NC}"; }

REPO="https://github.com/411A/V2ProDock.git"
INSTALL_DIR="$HOME/V2ProDock"

# Read a value from .env, returns default if missing
env_val() {
    local key="$1" default="$2"
    if [ -f "$DIR/.env" ]; then
        local v
        v=$(grep -E "^${key}=" "$DIR/.env" 2>/dev/null | head -1 | cut -d'=' -f2- | tr -d '[:space:]')
        [ -n "$v" ] && echo "$v" && return
    fi
    echo "$default"
}

# Print proxy info and health status after starting
show_status() {
    local port_base instances api_port
    port_base=$(env_val PORT_BASE 27019)
    instances=$(env_val PROXY_INSTANCES 1)
    api_port=$(env_val API_PORT 27018)

    echo ""
    echo -e "${CYAN}Proxies:${NC}"
    local i=0
    while [ "$i" -lt "$instances" ]; do
        local socks=$((port_base + i))
        local http=$((port_base + instances + i))
        echo "  SOCKS5: localhost:$socks   HTTP: localhost:$http"
        i=$((i + 1))
    done
    echo ""
    echo -e "${CYAN}Test (first proxy):${NC}"
    echo "  curl --socks5 localhost:$port_base https://api.ipify.org"
    echo "  curl --proxy http://localhost:$((port_base + instances)) https://api.ipify.org"
    echo ""

    # No waiting here — populate takes 30-90s in the background.
    # Just tell the user where to look.
    echo -e "${CYAN}Proxies are populating in the background (takes 30-90s). Check yourself:${NC}"
    echo "  Watch progress: docker logs -f v2prodock  (look for Ready lines)"
    echo "  Live proxy list: curl http://localhost:$api_port/proxies"
    echo "  Health: curl http://localhost:$api_port/health"
    echo ""
    echo "Other containers use:"
    echo "  HTTP_PROXY=http://v2prodock:$((port_base + instances))"
    echo "  HTTPS_PROXY=socks5://v2prodock:$port_base"
    echo ""
    local vpn_enabled vpn_domain vpn_user vpn_plain vpn_pptp
    vpn_enabled=$(env_val VPN_ENABLED 0)
    vpn_domain=$(env_val VPN_DOMAIN vpn.local)
    vpn_user=$(env_val VPN_USER vpnuser)
    vpn_plain=$(env_val VPN_ALLOW_PLAIN_L2TP 0)
    vpn_pptp=$(env_val VPN_ENABLE_PPTP 0)
    if [ "$vpn_enabled" = "1" ]; then
        echo -e "${CYAN}VPN for legacy devices (egress via working proxy):${NC}"
        echo "  L2TP/IPsec (no files needed): server $vpn_domain, IPsec PSK (see .env VPN_IPSEC_PSK), user $vpn_user"
        if [ "$vpn_plain" = "1" ]; then
            echo -e "  ${RED}Bare L2TP (no IPsec): ALLOWED - cleartext, for old stock firmware only${NC}"
        fi
        if [ "$vpn_pptp" = "1" ]; then
            echo -e "  ${RED}PPTP (ancient LAN devices): server $vpn_domain (TCP 1723+GRE), user $vpn_user, MPPE-128 mandatory - LAN-ONLY${NC}"
            echo "  Reconnects stall? See README PPTP section (docker-compose.host.yml host-networking fallback)"
        else
            echo "  PPTP: off (satellite receivers & other ancient LAN devices need VPN_ENABLE_PPTP=1 in .env, then: docker compose up -d)"
        fi
        echo "  IKEv2 (needs CA import):       server $vpn_domain (UDP 500/4500), user $vpn_user"
        echo "  Apple profile: $DIR/config/vpn/apple.mobileconfig"
        echo "  CA cert:       $DIR/config/vpn/ca.crt"
        echo "  Verify pinning: curl http://localhost:$api_port/vpn"
        echo "  Watch proofs:   docker logs -f v2prodock-vpn  (look for VERIFIED lines)"
    else
        echo -e "${CYAN}VPN:${NC} disabled (set VPN_ENABLED=1 + VPN_PASSWORD + VPN_IPSEC_PSK in .env to serve TVs/IoT)"
    fi
}

# If piped from curl or not inside repo, clone/pull first
if [ ! -f "v2proxy/main.go" ] || [ ! -f "docker-compose.yml" ]; then
    echo "Not inside V2ProDock repo. Setting up..."
    if [ -d "$INSTALL_DIR" ]; then
        cd "$INSTALL_DIR" || exit 1
        git pull --ff-only || {
            echo "Fast-forward failed (force push?), resetting..."
            git stash --include-untracked 2>/dev/null
            git fetch origin && git reset --hard origin/main
            git stash pop 2>/dev/null || true
        }
        ok "Updated $INSTALL_DIR"
    else
        git clone "$REPO" "$INSTALL_DIR" || { err "git clone failed"; exit 1; }
        cd "$INSTALL_DIR" || exit 1
        ok "Cloned to $INSTALL_DIR"
    fi
fi

# Always resolve DIR after possible cd
DIR="$(pwd)"

get_host_gateway_ip() {
    local ip
    ip=$(ip route show default 2>/dev/null | awk '/default/ {print $3; exit}')
    if [ -n "$ip" ]; then echo "$ip"; return; fi
    ip=$(ip route 2>/dev/null | awk '/default/ {print $3; exit}')
    if [ -n "$ip" ]; then echo "$ip"; return; fi
}

get_wsl_host_ip() {
    if grep -qiE "(microsoft|wsl)" /proc/version 2>/dev/null; then
        get_host_gateway_ip
    fi
}

fix_wsl_url() {
    local url="$1"
    if [[ "$url" == *"host.docker.internal"* ]] || [[ "$url" == *"127.0.0.1"* ]] || [[ "$url" == *"localhost"* ]]; then
        local gw
        gw=$(get_wsl_host_ip)
        if [ -n "$gw" ]; then
            echo "$url" | sed "s|host.docker.internal|$gw|g; s|127\.0\.0\.1|$gw|g; s|localhost|$gw|g"
            return
        fi
    fi
    echo "$url"
}

# Split a raw subscription list (comma/semicolon/whitespace/newline separated)
# into one URL per line, deduped, order preserved.
split_sub_urls() {
    printf '%s\n' "$1" | tr ',;' '\n' | awk '{ gsub(/^[ \t\r]+|[ \t\r]+$/, ""); if ($0 != "" && !seen[$0]++) print }'
}

# Read KEY from .env, supporting double-quoted multiline values.
dotenv_val() {
    local key="$1" file="$DIR/.env"
    [ -f "$file" ] || return 0
    awk -v k="$key" '
        !ingot && $0 ~ "^" k "=" {
            v = substr($0, length(k) + 2)
            sub(/\r$/, "", v)
            if (v ~ /^"/) {
                sub(/^"/, "", v)
                if (v ~ /"$/) { sub(/"$/, "", v); print v; exit }
                ingot = 1
                print v
                next
            }
            print v
            exit
        }
        ingot {
            line = $0
            sub(/\r$/, "", line)
            if (line ~ /"$/) { sub(/"$/, "", line); print line; exit }
            print line
        }
    ' "$file"
}

# Subscription URLs from .env only (both keys), one per line.
read_env_sub_urls() {
    local raw=""
    if [ -f "$DIR/.env" ]; then
        raw="$(dotenv_val SUBSCRIPTION_URLS)
$(dotenv_val SUBSCRIPTION_URL)"
    fi
    split_sub_urls "$raw"
}

# Subscription URLs from every source (file + .env + env vars), one per line.
read_sub_urls() {
    local raw=""
    if [ -f "$DIR/config/subscription.txt" ]; then
        raw="$raw
$(tr '\r' '\n' < "$DIR/config/subscription.txt")"
    fi
    if [ -f "$DIR/.env" ]; then
        raw="$raw
$(dotenv_val SUBSCRIPTION_URLS)
$(dotenv_val SUBSCRIPTION_URL)"
    fi
    [ -n "${SUBSCRIPTION_URLS:-}" ] && raw="$raw
$SUBSCRIPTION_URLS"
    [ -n "${SUBSCRIPTION_URL:-}" ] && raw="$raw
$SUBSCRIPTION_URL"
    split_sub_urls "$raw"
}

# Join a newline-separated URL list into one comma-separated line.
join_commas() {
    awk 'NF { if (n++) printf ","; printf "%s", $0 } END { if (n) printf "\n" }' <<< "$1"
}

# Rewrite loopback hosts in each URL of a newline-separated list.
rewrite_sub_urls() {
    local list="$1" out="" u f
    [ -z "$list" ] && return 0
    while IFS= read -r u; do
        [ -z "$u" ] && continue
        f=$(fix_wsl_url "$u")
        if [ "$f" = "$u" ]; then
            f=$(fix_vm_url "$u")
        fi
        if [ "$f" != "$u" ]; then
            ok "Rewriting $u -> $f"
        fi
        if [ -z "$out" ]; then out="$f"; else out="$out
$f"; fi
    done <<< "$list"
    printf '%s\n' "$out"
}

# Check each subscription URL separately; dead ones only warn (app has failover).
check_subscriptions_reachable() {
    local total=0 ok_count=0 u
    while IFS= read -r u; do
        [ -z "$u" ] && continue
        total=$((total + 1))
        if command -v curl &>/dev/null && curl -sf --max-time 5 "$u" >/dev/null 2>&1; then
            ok_count=$((ok_count + 1))
        else
            echo -e "${RED}[WARN] Cannot reach $u from host${NC}"
        fi
    done <<< "$1"
    [ "$total" -eq 0 ] && return 0
    if [ "$ok_count" -eq 0 ]; then
        echo "  None of the $total subscription URL(s) reachable from host."
        echo "  Inside Docker 127.0.0.1 = container, not host. Use:"
        echo "    http://host.docker.internal:27141/subscription  (compose has host-gateway)"
        echo "    http://$(hostname -I 2>/dev/null | awk '{print $1}'):27141/subscription  (LAN IP)"
        local gw; gw=$(get_host_gateway_ip)
        [ -n "$gw" ] && echo "    http://$gw:27141/subscription  (gateway IP)"
        echo "  The app skips dead sources at runtime if at least one works."
    else
        ok "$ok_count/$total subscription URL(s) reachable"
    fi
}

escape_sed_repl() { printf '%s' "$1" | sed -e 's/[&|\\]/\\&/g'; }

# Set KEY=VALUE in .env (adds if missing). Safe for URLs containing & | \.
set_env_key() {
    local key="$1" val="$2" esc
    esc=$(escape_sed_repl "$val")
    if grep -qE "^${key}=" "$DIR/.env" 2>/dev/null; then
        sed -i "s|^${key}=.*|${key}=${esc}|" "$DIR/.env"
    else
        printf '%s=%s\n' "$key" "$val" >> "$DIR/.env"
    fi
}

# Normalize subscription keys in .env: first URL -> SUBSCRIPTION_URL,
# rest -> quoted multiline SUBSCRIPTION_URLS. Drops stale key lines.
write_env_subscriptions() {
    local first="$1" rest="$2" tmp
    tmp=$(mktemp)
    awk -v first="$first" '
        /^SUBSCRIPTION_URL=/ { print "SUBSCRIPTION_URL=" first; replaced = 1; next }
        /^SUBSCRIPTION_URLS=".*"$/ { next }
        /^SUBSCRIPTION_URLS="/ { skip = 1; next }
        skip { if ($0 ~ /"/) skip = 0; next }
        /^SUBSCRIPTION_URLS=/ { next }
        { print }
        END { if (!replaced) print "SUBSCRIPTION_URL=" first }
    ' "$DIR/.env" > "$tmp" && mv "$tmp" "$DIR/.env"
    if [ -n "$rest" ]; then
        {
            echo 'SUBSCRIPTION_URLS="'
            printf '%s\n' "$rest"
            echo '"'
        } >> "$DIR/.env"
    fi
}

fix_vm_url() {
    local url="$1"
    if [[ "$url" == *"127.0.0.1"* ]] || [[ "$url" == *"localhost"* ]]; then
        if docker info 2>/dev/null | grep -q "host-gateway"; then
            echo "$url" | sed "s|127\.0\.0\.1|host.docker.internal|g; s|localhost|host.docker.internal|g"
            return
        fi
        local gw; gw=$(get_host_gateway_ip)
        if [ -n "$gw" ]; then
            echo "$url" | sed "s|127\.0\.0\.1|$gw|g; s|localhost|$gw|g"
            return
        fi
    fi
    echo "$url"
}

# Host-level prerequisites Docker cannot do itself: kernel modules the VPN
# container relies on, and host-firewall holes for the VPN UDP ports.
# Best-effort everywhere: missing tools only warn (README has manual steps).
ensure_host_prereqs() {
    local vpn_on="0"
    if [ -f "$DIR/.env" ]; then
        vpn_on=$(grep -E "^VPN_ENABLED=" "$DIR/.env" 2>/dev/null | head -1 | cut -d'=' -f2- | tr -d '[:space:]')
    fi

    # Containers cannot modprobe for themselves; no-ops when built-in.
    # ppp_async is the N_PPP line discipline - without it every pppd fails
    # with "Couldn't set tty to PPP discipline" (proven in E2E).
    if [ "$(id -u)" = "0" ]; then
        for mod in tun ppp_generic ppp_async ppp_mppe xt_policy nf_conntrack_pptp nf_nat_pptp; do
            modprobe "$mod" 2>/dev/null || true
        done
    fi
    if [ ! -c /dev/net/tun ]; then
        echo -e "${RED}[WARN] /dev/net/tun missing on this host - VPN cannot work (try: modprobe tun)${NC}"
    fi

    if [ "$vpn_on" != "1" ]; then
        return 0
    fi
    # Docker publishes the ports, but a default-deny host firewall still
    # blocks them before packets reach Docker. What each opening is for:
    #   UDP 500  - IKE handshake: opens every IPsec connection (IKEv2 and L2TP/IPsec alike)
    #   UDP 4500 - IPsec payloads, UDP-encapsulated (NAT-T): ALL IKEv2/L2TP bytes
    #              ride here, so raw ESP (IP protocol 50) is NOT required anywhere
    #   UDP 1701 - L2TP control/data: always wrapped in the IPsec above
    #              (bare L2TP is refused by default, opt-in only)
    #   TCP 1723 - PPTP control channel, ONLY when VPN_ENABLE_PPTP=1
    #   GRE      - PPTP data (IP protocol 47, NOT TCP/UDP, so no port rule can
    #              cover it - it needs a protocol rule). PPTP crypto is broken,
    #              so 1723+GRE stay confined to the LAN, never the internet.
    local pptp_on="0" n
    if [ -f "$DIR/.env" ]; then
        pptp_on=$(grep -E "^VPN_ENABLE_PPTP=" "$DIR/.env" 2>/dev/null | head -1 | cut -d'=' -f2- | tr -d '[:space:]')
    fi
    # Private ranges PPTP is confined to (RFC 1918 - covers any home/CGNAT LAN).
    local pptp_nets="192.168.0.0/16 10.0.0.0/8 172.16.0.0/12"
    if command -v ufw &>/dev/null; then
        if [ "$(id -u)" = "0" ]; then
            for p in 500 4500 1701; do ufw allow "$p/udp" >/dev/null 2>&1 || true; done
            ok "Host firewall (ufw): UDP 500 (IKE handshake) + 4500 (IPsec data) + 1701 (L2TP-in-IPsec) allowed"
            if [ "$pptp_on" = "1" ]; then
                for n in $pptp_nets; do
                    ufw allow from "$n" to any port 1723 proto tcp >/dev/null 2>&1 || true
                    ufw allow proto gre from "$n" >/dev/null 2>&1 || true
                done
                ok "Host firewall (ufw): PPTP confined to LAN (TCP 1723 + GRE from private ranges only)"
            fi
        else
            echo -e "${CYAN}Host firewall (ufw) is active - Docker's ports stay blocked until you open them:${NC}"
            echo "  sudo ufw allow 500/udp    # IKE handshake: opens every IPsec connection (IKEv2 + L2TP/IPsec)"
            echo "  sudo ufw allow 4500/udp   # IPsec payloads: all VPN bytes ride here (raw ESP proto 50 NOT needed)"
            echo "  sudo ufw allow 1701/udp   # L2TP control/data (travels inside the IPsec above)"
            if [ "$pptp_on" = "1" ]; then
                echo -e "  ${RED}PPTP is on (VPN_ENABLE_PPTP=1) but its crypto is broken - LAN-only, never the internet:${NC}"
                echo "  sudo ufw allow proto gre from 192.168.0.0/16  # PPTP data (IP proto 47, not a port)"
                echo "  sudo ufw allow proto gre from 10.0.0.0/8      # (one rule per private range you use)"
                echo "  sudo ufw allow proto gre from 172.16.0.0/12"
                echo "  sudo ufw allow from 192.168.0.0/16 to any port 1723 proto tcp  # PPTP control channel"
                echo "  sudo ufw allow from 10.0.0.0/8 to any port 1723 proto tcp"
                echo "  sudo ufw allow from 172.16.0.0/12 to any port 1723 proto tcp"
            fi
        fi
    elif command -v firewall-cmd &>/dev/null; then
        if [ "$(id -u)" = "0" ] && systemctl is-active --quiet firewalld 2>/dev/null; then
            for p in 500 4500 1701; do firewall-cmd --permanent --add-port="$p/udp" >/dev/null 2>&1 || true; done
            [ "$pptp_on" = "1" ] && firewall-cmd --permanent --add-port="1723/tcp" >/dev/null 2>&1 || true
            [ "$pptp_on" = "1" ] && firewall-cmd --permanent --add-protocol=gre >/dev/null 2>&1 || true
            firewall-cmd --reload >/dev/null 2>&1 || true
            ok "Host firewall (firewalld): UDP 500 (IKE) + 4500 (IPsec data) + 1701 (L2TP-in-IPsec) allowed$([ "$pptp_on" = "1" ] && echo ' + TCP 1723 and GRE (PPTP)')"
            if [ "$pptp_on" = "1" ]; then
                echo -e "${RED}[WARN] PPTP crypto is broken - confine 1723/GRE to your LAN with rich rules (see README), not the whole zone${NC}"
            fi
        else
            echo -e "${RED}[WARN] firewalld present - make sure these reach Docker:${NC}"
            echo "  UDP 500 (IKE handshake), UDP 4500 (IPsec data, ESP proto 50 NOT needed), UDP 1701 (L2TP-in-IPsec)"
            [ "$pptp_on" = "1" ] && echo -e "  ${RED}Plus LAN-only: TCP 1723 (PPTP control) + GRE proto 47 (PPTP data)${NC}"
        fi
    else
        echo "Host firewall: no ufw/firewalld found - if clients cannot connect, open these (host firewall + cloud security group):"
        echo "  UDP 500 (IKE handshake), UDP 4500 (IPsec data), UDP 1701 (L2TP-in-IPsec)"
        [ "$pptp_on" = "1" ] && echo -e "  ${RED}Plus LAN-only: TCP 1723 (PPTP control) + GRE proto 47 (PPTP data)${NC}"
    fi
    # GRE (IP proto 47) cannot be published via Docker ports:. Behind bridge
    # NAT the conntrack PPTP helper (modprobed above) forwards GRE alongside
    # the TCP 1723 DNAT. If PPTP control connects but traffic stalls, either
    # the helper is missing on this kernel or the cloud firewall drops GRE -
    # then run the vpn service with network_mode: host (Linux only).
    if [ "$pptp_on" = "1" ]; then
        if lsmod 2>/dev/null | grep -q nf_conntrack_pptp; then
            ok "GRE conntrack helper (nf_conntrack_pptp) loaded"
        else
            echo -e "${RED}[WARN] nf_conntrack_pptp not loaded - PPTP GRE may stall behind Docker NAT (host networking is the fallback)${NC}"
        fi
    fi
}



# Check if Docker is available
if command -v docker &>/dev/null && docker compose version &>/dev/null; then
    DOCKER_MODE=true
    ok "Docker detected"
else
    DOCKER_MODE=false
    echo "Docker not found, installing dependencies..."
fi

if [ "$DOCKER_MODE" = true ]; then
    # Docker mode
    case "${1:-}" in
        start)
            ensure_host_prereqs
            docker compose up -d
            ok "Started"
            show_status
            ;;
        stop)
            docker compose stop
            ok "Stopped"
            ;;
        status)
            docker compose ps
            echo ""
            docker logs --tail 10 v2prodock 2>&1
            echo ""
            echo "--- v2prodock-vpn (if enabled) ---"
            docker logs --tail 5 v2prodock-vpn 2>&1 || true
            curl -sf --max-time 5 http://localhost:$(env_val API_PORT 27018)/vpn 2>/dev/null || echo "(vpn status unavailable)"
            ;;
        logs)
            docker compose logs -f v2prodock
            ;;
        uninstall)
            read -p "Remove everything? [y/N]: " -n 1 -r; echo
            [[ ! $REPLY =~ ^[Yy]$ ]] && exit 0
            docker compose down -v
            rm -rf config .env
            ok "Removed"
            ;;
        *)
            # Install / update mode — always pull latest code first so a
            # stale checkout (e.g. re-running from inside the repo) can't
            # rebuild an old binary from Docker layer cache.
            if [ -d "$DIR/.git" ]; then
                if git -C "$DIR" pull --ff-only 2>&1; then
                    ok "Repo updated"
                else
                    echo -e "${RED}[WARN] git pull failed (local changes?) — continuing with local code${NC}"
                fi
            fi
            mkdir -p "$DIR/config"

            # 1. Gather subscription URLs from all sources (one per line)
            sub_urls=$(read_sub_urls)
            if [ -z "$sub_urls" ]; then
                echo "Enter subscription URL (comma-separated list allowed):"
                read -r -p "URL: " input
                [[ -z "$input" ]] && { err "URL required"; exit 1; }
                echo "$input" > "$DIR/config/subscription.txt"
                sub_urls=$(split_sub_urls "$input")
                ok "Subscription saved"
            fi

            # 2. Rewrite loopback hosts per URL (WSL2 / VM / Docker)
            fixed_urls=$(rewrite_sub_urls "$sub_urls")
            [ -n "$fixed_urls" ] && sub_urls="$fixed_urls"

            # 3. Reachability per URL (dead ones only warn — app has failover)
            check_subscriptions_reachable "$sub_urls"
            src_count=$(printf '%s\n' "$sub_urls" | grep -c .)
            ok "Using $src_count subscription source(s)"

            # 4. .env — copy from .env.example as template, then fill in user values
            first_url=$(printf '%s\n' "$sub_urls" | head -1)
            rest_urls=$(printf '%s\n' "$sub_urls" | tail -n +2)
            if [ ! -f "$DIR/.env" ]; then
                cp "$DIR/.env.example" "$DIR/.env"
                write_env_subscriptions "$first_url" "$rest_urls"

                echo ""
                echo -e "${CYAN}Configure additional settings (press Enter to keep defaults):${NC}"

                health_url="https://www.gstatic.com/generate_204"
                read -r -p "  Health check URL [$health_url]: " input; health_url=${input:-$health_url}
                set_env_key HEALTH_CHECK_URL "$health_url"

                read -r -p "  Number of proxy instances [1]: " input
                [ -n "$input" ] && set_env_key PROXY_INSTANCES "$input"

                read -r -p "  Enable VPN (IKEv2 + L2TP) for legacy devices? [y/N]: " input
                if [[ "$input" =~ ^[Yy]$ ]]; then
                    set_env_key VPN_ENABLED "1"
                    read -r -p "  VPN server domain/IP clients connect to [vpn.local]: " input
                    [ -n "$input" ] && set_env_key VPN_DOMAIN "$input"
                    read -r -p "  VPN username [vpnuser]: " input
                    [ -n "$input" ] && set_env_key VPN_USER "$input"
                    read -r -s -p "  VPN password (min 8 chars): " input; echo
                    [ -n "$input" ] && set_env_key VPN_PASSWORD "$input"
                    read -r -s -p "  IPsec PSK for L2TP (min 8 chars, Enter = auto-generate): " input; echo
                    if [ -z "$input" ]; then
                        if command -v openssl &>/dev/null; then
                            input=$(openssl rand -base64 18 | tr -d '\n')
                        else
                            input=$(head -c 18 /dev/urandom | od -An -tx1 | tr -d ' \n' | head -c 24)
                        fi
                        echo -e "${CYAN}  Generated IPsec PSK (save it — L2TP clients need it): $input${NC}"
                    fi
                    set_env_key VPN_IPSEC_PSK "$input"
                    read -r -p "  Enable PPTP for ancient LAN devices (MediaStar receivers)? [y/N]: " input
                    if [[ "$input" =~ ^[Yy]$ ]]; then
                        set_env_key VPN_ENABLE_PPTP "1"
                        echo -e "  ${RED}PPTP is cryptographically broken - keep it on a trusted LAN, never expose TCP 1723 to the internet.${NC}"
                        ok "PPTP enabled (TCP 1723 + GRE, MPPE-128 mandatory, egress pinned to working proxy)"
                    fi
                    ok "VPN enabled (IKEv2 UDP 500/4500 + L2TP UDP 1701$([ "$(env_val VPN_ENABLE_PPTP 0)" = "1" ] && echo ' + PPTP TCP 1723'), egress pinned to working proxy)"
                fi

                ok ".env created from .env.example"
                echo "  Edit $DIR/.env to customize further."
            else
                old_joined=$(join_commas "$(read_env_sub_urls)")
                new_joined=$(join_commas "$sub_urls")
                if [ "$new_joined" != "$old_joined" ]; then
                    write_env_subscriptions "$first_url" "$rest_urls"
                    ok "Updated subscriptions in .env"
                else
                    ok ".env subscriptions up to date"
                fi
                if [[ "$new_joined" == *"127.0.0.1"* ]] || [[ "$new_joined" == *"localhost"* ]]; then
                    echo -e "${RED}[WARN] .env still uses 127.0.0.1/localhost — inside Docker this fails. Use host.docker.internal or LAN IP${NC}"
                fi
            fi

            ensure_host_prereqs
            docker compose build 2>&1
            docker compose up -d 2>&1
            ok "Started"
            show_status
            ;;
    esac
else
    # Direct install mode (no Docker)
    if ! command -v go &>/dev/null; then
        echo "Installing Go..."
        curl -sL https://go.dev/dl/go1.25.4.linux-amd64.tar.gz | sudo tar -C /usr/local -xzf -
        echo "export PATH=\$PATH:/usr/local/go/bin" >> ~/.bashrc
        export PATH=$PATH:/usr/local/go/bin
        ok "Go installed"
    fi

    command -v unzip &>/dev/null || sudo apt-get install -y unzip

    if ! command -v zellij &>/dev/null; then
        echo "Installing zellij..."
        curl -sL https://github.com/zellij-org/zellij/releases/latest/download/zellij-x86_64-unknown-linux-musl.tar.gz | sudo tar -C /usr/local/bin -xzf -
        chmod +x /usr/local/bin/zellij
        ok "Zellij installed"
    fi

    if [ ! -f "$DIR/xray/xray" ]; then
        echo "Downloading xray..."
        mkdir -p "$DIR/xray"
        ARCH=$(uname -m)
        case "$ARCH" in
            x86_64)  XARCH="64" ;;
            aarch64) XARCH="arm64-v8a" ;;
            armv7l)  XARCH="arm32-v7a" ;;
            *)       XARCH="64" ;;
        esac
        curl -sL "https://github.com/XTLS/Xray-core/releases/latest/download/Xray-linux-${XARCH}.zip" -o /tmp/x.zip
        unzip -o /tmp/x.zip -d "$DIR/xray" && rm /tmp/x.zip
        chmod +x "$DIR/xray/xray"
        ok "Xray downloaded"
    fi

    if [ ! -f "$DIR/v2proxy" ] || [ -n "$(find v2proxy/ -newer v2proxy -maxdepth 0 2>/dev/null)" ]; then
        echo "Building v2proxy..."
        cd "$DIR/v2proxy" || exit 1
        go build -o "$DIR/v2proxy" .
        cd "$DIR" || exit 1
        ok "Built"
    fi

    mkdir -p "$DIR/config"

    # Get subscription URLs from any available source
    sub_urls=$(read_sub_urls)

    if [ -z "$sub_urls" ]; then
        echo "Enter subscription URL (comma-separated list allowed):"
        read -r -p "URL: " input
        [[ -z "$input" ]] && { err "URL required"; exit 1; }
        echo "$input" > "$DIR/config/subscription.txt"
        sub_urls=$(split_sub_urls "$input")
        ok "Subscription saved"
    fi
    src_count=$(printf '%s\n' "$sub_urls" | grep -c .)
    ok "Using $src_count subscription source(s)"

    health_url="https://www.gstatic.com/generate_204"
    echo "Health check URL (default: $health_url):"
    read -r -p "URL: " input; health_url=${input:-$health_url}

    ok "Config ready"

    zellij kill-session v2proxy 2>/dev/null || true

    local_port_base=$(env_val PORT_BASE 27019)
    local_instances=$(env_val PROXY_INSTANCES 1)

    echo ""
    echo -e "${CYAN}Starting V2Ray Proxy in zellij session 'v2proxy'...${NC}"
    echo ""
    echo -e "${CYAN}Proxies:${NC}"
    i=0
    while [ "$i" -lt "$local_instances" ]; do
        s=$((local_port_base + i))
        h=$((local_port_base + local_instances + i))
        echo "  SOCKS5: localhost:$s   HTTP: localhost:$h"
        i=$((i + 1))
    done
    echo ""
    echo "  Attach:  zellij attach v2proxy"
    echo "  Detach:  Ctrl+O then D"
    echo ""

    export SUBSCRIPTION_URL="$(join_commas "$sub_urls")"
    export HEALTH_CHECK_URL="$health_url"
    export XRAY_DIR="$DIR/xray"

    zellij --session v2proxy -- "$DIR/v2proxy"
fi
