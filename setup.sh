#!/bin/bash
set -uo pipefail

RED='\033[0;31m'; GREEN='\033[0;32m'; CYAN='\033[0;36m'; NC='\033[0m'
ok() { echo -e "${GREEN}[OK] $1${NC}"; }
err() { echo -e "${RED}[ERROR] $1${NC}"; }

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$DIR" || exit 1

# Check if Docker is available
if command -v docker &>/dev/null && docker compose version &>/dev/null; then
    DOCKER_MODE=true
    ok "Docker detected"

    # Configure Docker mirror for Iran (bypass Docker Hub block)
    MIRROR_CONFIG="/etc/docker/daemon.json"
    if ! sudo grep -q "registry-mirrors" "$MIRROR_CONFIG" 2>/dev/null; then
        echo "Configuring Docker mirror for Iran..."
        sudo mkdir -p /etc/docker
        sudo bash -c 'cat > /etc/docker/daemon.json << EOF
{
  "registry-mirrors": [
    "https://docker.m.daocloud.io",
    "https://docker.registry.cyou",
    "https://docker.1panel.live"
  ]
}
EOF'
        sudo systemctl daemon-reload
        sudo systemctl restart docker
        ok "Docker mirror configured"
    fi
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
            echo ""
            echo -e "${CYAN}Proxies (Docker bridge):${NC}"
            echo "  SOCKS5: v2proxy:27019 (from other containers)"
            echo "  HTTP:   v2proxy:27020 (from other containers)"
            echo ""
            echo -e "${CYAN}Proxies (host):${NC}"
            echo "  SOCKS5: localhost:27019"
            echo "  HTTP:   localhost:27020"
            echo ""
            echo "Usage in other containers:"
            echo "  network_mode: service:v2proxy"
            echo "  OR"
            echo "  environment:"
            echo "    - HTTP_PROXY=http://v2proxy:27020"
            echo "    - HTTPS_PROXY=socks5://v2proxy:27019"
            ;;
        stop)
            docker compose stop
            ok "Stopped"
            ;;
        status)
            docker compose ps
            echo ""
            docker logs --tail 10 v2proxy 2>&1
            ;;
        logs)
            docker compose logs -f v2proxy
            ;;
        uninstall)
            read -p "Remove everything? [y/N]: " -n 1 -r; echo
            [[ ! $REPLY =~ ^[Yy]$ ]] && exit 0
            docker compose down -v
            rm -rf config .env
            ok "Removed"
            ;;
        *)
            # Install mode
            mkdir -p config

            if [ -f "$DIR/config/subscription.txt" ]; then
                sub_url=$(cat "$DIR/config/subscription.txt")
                echo "Current subscription: $sub_url"
                read -p "Keep? [Y/n]: " -n 1 -r; echo
                [[ $REPLY =~ ^[Nn]$ ]] && sub_url=""
            fi

            if [ -z "$sub_url" ]; then
                echo "Enter subscription URL:"
                read -r -p "URL: " sub_url
                [[ -z "$sub_url" ]] && { err "URL required"; exit 1; }
                echo "$sub_url" > "$DIR/config/subscription.txt"
            fi

            health_url="http://api.ipify.org"
            echo "Health check URL (default: $health_url):"
            read -r -p "URL: " input; health_url=${input:-$health_url}

            echo -e "SUBSCRIPTION_URL=$sub_url\nHEALTH_CHECK_URL=$health_url" > .env
            ok "Config ready"

            docker compose build 2>&1
            docker compose up -d 2>&1
            ok "Started"
            echo ""
            echo -e "${CYAN}Proxies:${NC}"
            echo "  SOCKS5: localhost:27019"
            echo "  HTTP:   localhost:27020"
            echo ""
            echo "Test:"
            echo "  curl --socks5 localhost:27019 https://api.ipify.org"
            echo "  curl --proxy http://localhost:27020 https://api.ipify.org"
            echo ""
            echo "Other containers use:"
            echo "  HTTP_PROXY=http://v2proxy:27020"
            echo "  HTTPS_PROXY=socks5://v2proxy:27019"
            ;;
    esac
else
    # Direct install mode (no Docker)
    # Install Go if not present
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

    if [ -f "$DIR/config/subscription.txt" ]; then
        sub_url=$(cat "$DIR/config/subscription.txt")
        echo "Current subscription: $sub_url"
        read -r -p "Keep? [Y/n]: " -n 1 -r; echo
        [[ $REPLY =~ ^[Nn]$ ]] && sub_url=""
    fi

    if [ -z "$sub_url" ]; then
        echo "Enter subscription URL:"
        read -r -p "URL: " sub_url
        [[ -z "$sub_url" ]] && { err "URL required"; exit 1; }
        echo "$sub_url" > "$DIR/config/subscription.txt"
    fi

    health_url="http://api.ipify.org"
    echo "Health check URL (default: $health_url):"
    read -r -p "URL: " input; health_url=${input:-$health_url}

    ok "Config ready"

    zellij kill-session v2proxy 2>/dev/null || true

    echo ""
    echo -e "${CYAN}Starting V2Ray Proxy in zellij session 'v2proxy'...${NC}"
    echo ""
    echo "  SOCKS5: localhost:27019"
    echo "  HTTP:   localhost:27020"
    echo ""
    echo "  Attach:  zellij attach v2proxy"
    echo "  Detach:  Ctrl+O then D"
    echo ""

    export SUBSCRIPTION_URL="$sub_url"
    export HEALTH_CHECK_URL="$health_url"
    export XRAY_DIR="$DIR/xray"

    zellij --session v2proxy -- "$DIR/v2proxy"
fi
