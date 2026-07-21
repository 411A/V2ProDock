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
        local socks=$((port_base + i * 2))
        local http=$((socks + 1))
        echo "  SOCKS5: localhost:$socks   HTTP: localhost:$http"
        i=$((i + 1))
    done
    echo ""
    echo -e "${CYAN}Test (first proxy):${NC}"
    echo "  curl --socks5 localhost:$port_base https://api.ipify.org"
    echo "  curl --proxy http://localhost:$((port_base + 1)) https://api.ipify.org"
    echo ""

    # Check API health after a brief wait
    sleep 3
    local health
    health=$(curl -sf "http://localhost:$api_port/health" 2>/dev/null)
    if [ -n "$health" ]; then
        local alive total
        alive=$(echo "$health" | grep -o '"alive":[0-9]*' | cut -d: -f2)
        total=$(echo "$health" | grep -o '"instances":[0-9]*' | cut -d: -f2)
        if [ "${alive:-0}" -gt 0 ]; then
            ok "Proxy healthy: $alive/$total instances alive"
        else
            err "All $total instances are DOWN — check subscription URL in .env"
            echo "  Edit: $DIR/.env"
            echo "  Logs: docker logs v2prodock"
        fi
    else
        echo -e "${CYAN}API not ready yet — run 'docker logs v2prodock' to check status${NC}"
    fi
    echo ""
    echo "Other containers use:"
    echo "  HTTP_PROXY=http://v2prodock:$((port_base + 1))"
    echo "  HTTPS_PROXY=socks5://v2prodock:$port_base"
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

# Detect WSL2 and get Windows host IP
get_wsl_host_ip() {
    if grep -qiE "(microsoft|wsl)" /proc/version 2>/dev/null; then
        # Use default gateway — always the Windows host in WSL2
        local ip
        ip=$(ip route show default 2>/dev/null | awk '/default/ {print $3}')
        if [ -n "$ip" ]; then
            echo "$ip"
            return
        fi
    fi
}

# Fix subscription URL for WSL2 (host.docker.internal doesn't reach Windows host)
fix_wsl_url() {
    local url="$1"
    if [[ "$url" == *"host.docker.internal"* ]] || [[ "$url" == *"127.0.0.1"* ]]; then
        local win_ip
        win_ip=$(get_wsl_host_ip)
        if [ -n "$win_ip" ]; then
            local fixed
            fixed=$(echo "$url" | sed "s|host.docker.internal|$win_ip|g; s|127.0.0.1|$win_ip|g")
            echo "$fixed"
            return
        fi
    fi
    echo "$url"
}

# Read subscription URL from a source, stripping CRLF and whitespace
read_sub_url() {
    local url=""
    # Source 1: config/subscription.txt
    if [ -f "$DIR/config/subscription.txt" ]; then
        url=$(tr -d '\r' < "$DIR/config/subscription.txt" | tr -d '[:space:]')
        if [ -n "$url" ]; then
            echo "$url"
            return
        fi
    fi
    # Source 2: .env file
    if [ -f "$DIR/.env" ]; then
        url=$(tr -d '\r' < "$DIR/.env" | grep -E '^SUBSCRIPTION_URL=' | head -1 | cut -d'=' -f2- | tr -d '[:space:]')
        if [ -n "$url" ]; then
            echo "$url"
            return
        fi
    fi
    # Source 3: env var
    if [ -n "${SUBSCRIPTION_URL:-}" ]; then
        echo "$SUBSCRIPTION_URL"
        return
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
            # Install / update mode
            mkdir -p "$DIR/config"

            # Get subscription URL from any available source
            sub_url=$(read_sub_url)

            # Auto-fix WSL2 networking: replace host.docker.internal with Windows host IP
            fixed_url=$(fix_wsl_url "$sub_url")
            if [ "$fixed_url" != "$sub_url" ]; then
                win_ip=$(get_wsl_host_ip)
                ok "WSL2 detected — replacing host.docker.internal with $win_ip"
                sub_url="$fixed_url"
            fi

            if [ -n "$sub_url" ]; then
                ok "Found subscription: $sub_url"
            else
                echo "Enter subscription URL:"
                read -r -p "URL: " sub_url
                [[ -z "$sub_url" ]] && { err "URL required"; exit 1; }
                echo "$sub_url" > "$DIR/config/subscription.txt"
                ok "Subscription saved"
            fi

            # .env — copy from .env.example as template, then fill in user values
            if [ ! -f "$DIR/.env" ]; then
                cp "$DIR/.env.example" "$DIR/.env"

                # Fill in the subscription URL the user provided
                sed -i "s|^SUBSCRIPTION_URL=.*|SUBSCRIPTION_URL=$sub_url|" "$DIR/.env"

                echo ""
                echo -e "${CYAN}Configure additional settings (press Enter to keep defaults):${NC}"

                health_url="http://api.ipify.org"
                read -r -p "  Health check URL [$health_url]: " input; health_url=${input:-$health_url}
                sed -i "s|^HEALTH_CHECK_URL=.*|HEALTH_CHECK_URL=$health_url|" "$DIR/.env"

                read -r -p "  Number of proxy instances [1]: " input
                [ -n "$input" ] && sed -i "s|^PROXY_INSTANCES=.*|PROXY_INSTANCES=$input|" "$DIR/.env"

                ok ".env created from .env.example"
                echo "  Edit $DIR/.env to customize further."
            else
                ok ".env exists, skipping"
                # Fix WSL2 URL in existing .env
                if [ "$fixed_url" != "$(read_sub_url)" ]; then
                    sed -i "s|^SUBSCRIPTION_URL=.*|SUBSCRIPTION_URL=$fixed_url|" "$DIR/.env"
                    ok "Updated SUBSCRIPTION_URL in .env for WSL2"
                fi
            fi

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
        curl -sL https://go.dev/dl/go1.23.4.linux-amd64.tar.gz | sudo tar -C /usr/local -xzf -
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

    # Get subscription URL from any available source
    sub_url=$(read_sub_url)

    if [ -n "$sub_url" ]; then
        ok "Found subscription: $sub_url"
    else
        echo "Enter subscription URL:"
        read -r -p "URL: " sub_url
        [[ -z "$sub_url" ]] && { err "URL required"; exit 1; }
        echo "$sub_url" > "$DIR/config/subscription.txt"
        ok "Subscription saved"
    fi

    health_url="http://api.ipify.org"
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
        s=$((local_port_base + i * 2))
        h=$((s + 1))
        echo "  SOCKS5: localhost:$s   HTTP: localhost:$h"
        i=$((i + 1))
    done
    echo ""
    echo "  Attach:  zellij attach v2proxy"
    echo "  Detach:  Ctrl+O then D"
    echo ""

    export SUBSCRIPTION_URL="$sub_url"
    export HEALTH_CHECK_URL="$health_url"
    export XRAY_DIR="$DIR/xray"

    zellij --session v2proxy -- "$DIR/v2proxy"
fi
