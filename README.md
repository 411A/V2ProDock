# V2Ray Proxy

Xray-core based proxy client. Parses subscription URLs, health-checks proxies, auto-switches to working ones.

## Quick Start

```bash
git clone <your-repo> && cd V2RayInsideDocker
sudo bash setup.sh
```

Enter your subscription URL when prompted.

## Usage

### Direct (from host)

```bash
# SOCKS5
curl --socks5 localhost:27019 https://api.ipify.org

# HTTP
curl --proxy http://localhost:27020 https://api.ipify.org
```

### From other Docker containers

```yaml
# docker-compose.yml
services:
  your-app:
    image: your-app
    environment:
      - HTTP_PROXY=http://v2proxy:27020
      - HTTPS_PROXY=socks5://v2proxy:27019
      - ALL_PROXY=socks5://v2proxy:27019
    networks:
      - proxy-net

networks:
  proxy-net:
    external: true
    name: v2docker_proxy-net
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

### From curl

```bash
curl --proxy http://localhost:27020 https://api.ipify.org
curl --socks5 localhost:27019 https://api.ipify.org
```

### System-wide proxy (Linux)

```bash
# Add to ~/.bashrc
export HTTP_PROXY=http://localhost:27020
export HTTPS_PROXY=socks5://localhost:27019
export ALL_PROXY=socks5://localhost:27019
```

## Proxies

| Type | Port | Usage |
|------|------|-------|
| SOCKS5 | 27019 | `socks5://localhost:27019` |
| HTTP | 27020 | `http://localhost:27020` |

## Commands

```bash
sudo bash setup.sh           # Install & start
sudo bash setup.sh start     # Start
sudo bash setup.sh stop      # Stop
sudo bash setup.sh status    # Status
sudo bash setup.sh logs      # Follow logs
```

## How It Works

1. Fetches subscription, parses vless/vmess/trojan/ss URLs
2. Converts to xray-core JSON configs
3. Starts xray providing SOCKS5 + HTTP proxies
4. Health-checks every 60s, auto-switches on failure
5. Refreshes subscription every 120s

## Supported Protocols

vless (tcp/ws/grpc/reality), vmess, trojan, shadowsocks
