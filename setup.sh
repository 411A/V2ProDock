#!/bin/bash
set -uo pipefail

RED='\033[0;31m'; GREEN='\033[0;32m'; CYAN='\033[0;36m'; NC='\033[0m'
ok() { echo -e "${GREEN}[OK] $1${NC}"; }
err() { echo -e "${RED}[ERROR] $1${NC}"; }

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$DIR"

# Install Go if not present
if ! command -v go &>/dev/null; then
    echo "Installing Go..."
    curl -sL https://go.dev/dl/go1.23.4.linux-amd64.tar.gz | sudo tar -C /usr/local -xzf -
    echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
    export PATH=$PATH:/usr/local/go/bin
    ok "Go installed"
fi

# Install unzip if not present
command -v unzip &>/dev/null || sudo apt-get install -y unzip

# Install zellij if not present
if ! command -v zellij &>/dev/null; then
    echo "Installing zellij..."
    curl -sL https://github.com/zellij-org/zellij/releases/latest/download/zellij-x86_64-unknown-linux-musl.tar.gz | tar -C /usr/local/bin -xzf -
    chmod +x /usr/local/bin/zellij
    ok "Zellij installed"
fi

# Download xray if not present
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

# Build Go binary if not present or source changed
if [ ! -f "$DIR/v2proxy" ] || [ -n "$(find v2proxy/ -newer v2proxy -maxdepth 0 2>/dev/null)" ]; then
    echo "Building v2proxy..."
    cd "$DIR/v2proxy"
    go build -o "$DIR/v2proxy" .
    cd "$DIR"
    ok "Built"
fi

# Create config dir
mkdir -p "$DIR/config"

# Get subscription URL
if [ -f "$DIR/config/subscription.txt" ]; then
    sub_url=$(cat "$DIR/config/subscription.txt")
    echo "Current subscription: $sub_url"
    read -p "Keep? [Y/n]: " -n 1 -r; echo
    [[ $REPLY =~ ^[Nn]$ ]] && sub_url=""
fi

if [ -z "$sub_url" ]; then
    echo "Enter subscription URL:"
    read -p "URL: " sub_url
    [[ -z "$sub_url" ]] && { err "URL required"; exit 1; }
    echo "$sub_url" > "$DIR/config/subscription.txt"
fi

# Get health check URL
health_url="http://api.ipify.org"
echo "Health check URL (default: $health_url):"
read -p "URL: " input; health_url=${input:-$health_url}

ok "Config ready"

# Kill existing session if running
zellij kill-session v2proxy 2>/dev/null || true

# Start in zellij session
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
