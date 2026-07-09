#!/bin/bash
set -uo pipefail

RED='\033[0;31m'; GREEN='\033[0;32m'; CYAN='\033[0;36m'; NC='\033[0m'
ok() { echo -e "${GREEN}[OK] $1${NC}"; }
err() { echo -e "${RED}[ERROR] $1${NC}"; }

[[ $EUID -ne 0 ]] && { err "Run as root"; exit 1; }
command -v docker &>/dev/null || { err "Docker not installed"; exit 1; }
docker compose version &>/dev/null || { err "Docker Compose not available"; exit 1; }

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$DIR"

case "${1:-}" in
    start)  docker compose up -d; ok "Started" ;;
    stop)   docker compose stop; ok "Stopped" ;;
    status) docker compose ps; echo; docker logs --tail 10 v2proxy 2>&1 ;;
    uninstall)
        read -p "Remove everything? [y/N]: " -n 1 -r; echo
        [[ ! $REPLY =~ ^[Yy]$ ]] && exit 0
        docker compose down -v; rm -rf config .env; ok "Removed" ;;
    *)
        mkdir -p config
        echo "Enter subscription URL:"
        read -p "URL: " sub_url
        [[ -z "$sub_url" ]] && { err "URL required"; exit 1; }
        echo "$sub_url" > config/subscription.txt
        echo "Health check URL (default: http://httpbin.org/ip):"
        read -p "URL: " health_url; health_url=${health_url:-"http://httpbin.org/ip"}
        echo -e "SUBSCRIPTION_URL=$sub_url\nHEALTH_CHECK_URL=$health_url" > .env
        ok "Config saved"
        docker compose build 2>&1
        docker compose up -d 2>&1
        ok "Started"
        echo ""
        echo -e "${CYAN}Proxies:${NC}"
        echo "  SOCKS5: localhost:27019"
        echo "  HTTP:   localhost:27020"
        echo ""
        echo "Test:"
        echo "  curl --socks5 localhost:27019 http://httpbin.org/ip"
        echo "  curl --proxy http://localhost:27020 http://httpbin.org/ip"
        ;;
esac
