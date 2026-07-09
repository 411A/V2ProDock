# V2Ray Proxy

Xray-core based proxy client. Parses subscription URLs, health-checks proxies, auto-switches to working ones.

## Quick Start (Ubuntu 24.04 VPS)

```bash
git clone <your-repo> && cd V2RayInsideDocker
sudo bash setup-v2prodock.sh
```

Enter your subscription URL when prompted. The script:
1. Detects OS/arch, downloads xray-core automatically
2. Builds the Go binary
3. Starts the proxy container

## Usage

```bash
# SOCKS5
curl --socks5 127.0.0.1:1080 https://api.ipify.org

# HTTP
curl --proxy http://127.0.0.1:27019 https://api.ipify.org
```

## Commands

```bash
sudo bash setup-v2prodock.sh start      # Start
sudo bash setup-v2prodock.sh stop       # Stop
sudo bash setup-v2prodock.sh status     # Status
sudo bash setup-v2prodock.sh uninstall  # Remove
```

## How It Works

1. Fetches subscription, parses vless/vmess/trojan/ss URLs
2. Converts to xray-core JSON configs
3. Starts xray providing SOCKS5 (:1080) + HTTP (:27019)
4. Health-checks every 60s, auto-switches on failure
5. Refreshes subscription every 120s

## Deploy to VPS

```bash
# On your VPS (Ubuntu 24.04)
sudo apt update && sudo apt install -y git docker.io docker-compose-v2
git clone <your-repo> && cd V2RayInsideDocker
sudo bash setup-v2prodock.sh
```

Other apps on the VPS can use the proxy:
```bash
export HTTP_PROXY=http://127.0.0.1:27019
export HTTPS_PROXY=socks5://127.0.0.1:1080
```

## Supported Protocols

vless (tcp/ws/grpc), vmess, trojan, shadowsocks
