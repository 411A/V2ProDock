# V2ProDock

A Dockerized V2Ray/Xray proxy client that manages the entire proxy lifecycle — from subscription parsing to automatic failover. Feed it a subscription URL, and it handles the rest: parses protocols, health-checks servers, and provides stable SOCKS5 + HTTP proxies for your other apps and containers.

## Features

- **Subscription-based** — paste a v2ray subscription URL, it parses vless/vmess/trojan/shadowsocks configs automatically
- **Auto-failover** — health-checks every 60s, switches to the next working server on failure
- **Auto-refresh** — re-fetches subscription every 120s for updated server lists
- **Dual proxies** — exposes SOCKS5 (port 27019) and HTTP (port 27020) for any app
- **Docker bridge** — other containers connect through the Docker network without port mapping
- **Multi-platform** — auto-detects OS/arch and downloads the right xray-core binary
- **Dynamic ports** — if the default port is occupied, automatically finds the next available one

## Quick Start

```bash
git clone https://github.com/YOUR_USER/V2ProDock.git && cd V2ProDock
sudo bash setup.sh
```

Enter your subscription URL when prompted. Done.

## Usage

### From the host machine

```bash
# SOCKS5
curl --socks5 localhost:27019 https://api.ipify.org

# HTTP
curl --proxy http://localhost:27020 https://api.ipify.org
```

### From other Docker containers

```yaml
services:
  your-app:
    image: your-app
    environment:
      - HTTP_PROXY=http://v2prodock:27020
      - HTTPS_PROXY=socks5://v2prodock:27019
      - ALL_PROXY=socks5://v2prodock:27019
    networks:
      - proxy-net

networks:
  proxy-net:
    external: true
    name: v2prodock_v2prodock-proxy-net
```

### From Python

```python
import requests

proxies = {
    "http": "http://localhost:27020",
    "https": "socks5://localhost:27019",
}

r = requests.get("https://api.ipify.org", proxies=proxies)
print(r.text)  # Shows proxy IP, not your real IP
```

### System-wide (Linux)

```bash
export HTTP_PROXY=http://localhost:27020
export HTTPS_PROXY=socks5://localhost:27019
export ALL_PROXY=socks5://localhost:27019
```

## Commands

```bash
sudo bash setup.sh           # Install & start
sudo bash setup.sh start     # Start
sudo bash setup.sh stop      # Stop
sudo bash setup.sh status    # Show status
sudo bash setup.sh logs      # Follow logs
sudo bash setup.sh uninstall # Remove everything
```

## How It Works

1. Fetches subscription URL and parses vless/vmess/trojan/shadowsocks links
2. Converts each to an xray-core JSON outbound config
3. Tests each config against a health-check URL via the SOCKS5 proxy
4. Starts xray with the first working config, exposing SOCKS5 + HTTP proxies
5. Runs periodic health checks (60s) — on failure, switches to the next working server
6. Re-fetches the subscription periodically (120s) for updated server lists

## Supported Protocols

| Protocol | Transport |
|----------|-----------|
| VLESS | TCP, WebSocket, gRPC, Reality |
| VMess | TCP, WebSocket, gRPC |
| Trojan | TCP, WebSocket |
| Shadowsocks | TCP |

## Configuration

Environment variables (set in `.env` or via docker-compose):

| Variable | Default | Description |
|----------|---------|-------------|
| `SUBSCRIPTION_URL` | — | Your v2ray subscription URL |
| `HEALTH_CHECK_URL` | `http://api.ipify.org` | URL used to test proxy connectivity |

## License

MIT
