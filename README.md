# V2Ray Proxy

Xray-core based proxy client. Parses subscription URLs, health-checks proxies, auto-switches to working ones.

## Quick Start (Ubuntu 24.04 VPS)

```bash
git clone <your-repo> && cd V2RayInsideDocker
sudo bash setup.sh
```

Enter your subscription URL when prompted. The script:
1. Installs Go if not present
2. Downloads xray-core for your architecture
3. Builds the Go binary
4. Starts the proxy

## Usage

```bash
# SOCKS5
curl --socks5 127.0.0.1:27019 https://api.ipify.org

# HTTP
curl --proxy http://127.0.0.1:27020 https://api.ipify.org
```

## How It Works

1. Fetches subscription, parses vless/vmess/trojan/ss URLs
2. Converts to xray-core JSON configs
3. Starts xray providing SOCKS5 (:27019) + HTTP (:27020)
4. Health-checks every 60s, auto-switches on failure
5. Refreshes subscription every 120s

## Deploy to VPS

```bash
# On your VPS (Ubuntu 24.04)
sudo apt update && sudo apt install -y git curl unzip
git clone <your-repo> && cd V2RayInsideDocker
sudo bash setup.sh
```

Other apps on the VPS can use the proxy:
```bash
export HTTP_PROXY=http://127.0.0.1:27020
export HTTPS_PROXY=socks5://127.0.0.1:27019
```

## Supported Protocols

vless (tcp/ws/grpc), vmess, trojan, shadowsocks
