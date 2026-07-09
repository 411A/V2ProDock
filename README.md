# V2Ray Proxy

Xray-core based proxy client. Parses subscription URLs, health-checks proxies, auto-switches to working ones.

## Quick Start (Ubuntu 24.04 VPS)

```bash
git clone <your-repo> && cd V2RayInsideDocker
sudo bash setup.sh
```

Enter your subscription URL when prompted.

## Docker Bridge Mode (Recommended)

The proxy runs in Docker and provides a bridge for other containers:

```bash
# Start the proxy
sudo bash setup.sh

# Test it
curl --socks5 localhost:27019 https://api.ipify.org
curl --proxy http://localhost:27020 https://api.ipify.org
```

### Use from other containers

```yaml
# docker-compose.yml
services:
  app:
    image: your-app
    environment:
      - HTTP_PROXY=http://v2proxy:27020
      - HTTPS_PROXY=socks5://v2proxy:27019
    networks:
      - proxy-net

networks:
  proxy-net:
    external: true
    name: v2docker_proxy-net
```

### Or use host network mode

```yaml
services:
  app:
    image: your-app
    network_mode: service:v2proxy
```

## Direct Install (No Docker)

```bash
sudo bash setup.sh
# Select "n" when asked about Docker

# Attach to session
zellij attach v2proxy

# Detach: Ctrl+O then D
```

## Proxies

| Type | Port | Docker Bridge | Host |
|------|------|---------------|------|
| SOCKS5 | 27019 | `v2proxy:27019` | `localhost:27019` |
| HTTP | 27020 | `v2proxy:27020` | `localhost:27020` |

## How It Works

1. Fetches subscription, parses vless/vmess/trojan/ss URLs
2. Converts to xray-core JSON configs
3. Starts xray providing SOCKS5 + HTTP proxies
4. Health-checks every 60s, auto-switches on failure
5. Refreshes subscription every 120s

## Supported Protocols

vless (tcp/ws/grpc/reality), vmess, trojan, shadowsocks
