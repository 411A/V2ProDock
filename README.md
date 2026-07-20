# V2ProDock

A Dockerized V2Ray/Xray proxy client that manages the entire proxy lifecycle — from subscription parsing to automatic failover. Feed it a subscription URL, and it handles the rest: parses protocols, health-checks servers, and provides stable SOCKS5 + HTTP proxies for your other apps and containers.

## Features

- **Subscription-based** — paste a v2ray subscription URL, it parses vless/vmess/trojan/shadowsocks configs automatically
- **Multi-instance** — run N independent xray processes, each with its own proxy pair and failover
- **Auto-failover** — health-checks every 60s, switches to the next working server on failure
- **Auto-refresh** — re-fetches subscription every 120s for updated server lists
- **HTTP API** — query live proxies sorted by latency at `GET /proxies`
- **Dynamic ports** — all ports are auto-assigned, no hardcoded ranges
- **Docker bridge** — other containers connect through the Docker network without port mapping
- **Multi-platform** — auto-detects OS/arch and downloads the right xray-core binary
- **Low-end friendly** — tunable memory limits, connection caps, and GC tuning for constrained devices

## Quick Start

```bash
git clone https://github.com/411A/V2ProDock.git && cd V2ProDock
cp .env.example .env
# Edit .env with your subscription URL(s)
sudo bash setup.sh
```

## Multi-Instance Setup

Run multiple independent proxy instances in a single container:

```bash
# .env
SUBSCRIPTION_URLS=https://sub1.example,https://sub2.example,https://sub3.example
PROXY_INSTANCES=3
```

Each instance gets its own dynamically assigned SOCKS5 + HTTP port pair. Query them via the API:

```bash
# Returns alive proxies sorted by lowest latency
curl http://localhost:27018/proxies
```

```json
[
  {"index":1, "socks5":"0.0.0.0:27021", "http":"0.0.0.0:27022", "status":"ok", "latency_ms":85, "name":"server-1"},
  {"index":0, "socks5":"0.0.0.0:27019", "http":"0.0.0.0:27020", "status":"ok", "latency_ms":120, "name":"server-2"}
]
```

### API Endpoints

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/proxies` | GET | Alive proxies sorted by latency (lowest first) |
| `/all` | GET | All instances including down ones |
| `/health` | GET | `{"status":"ok","instances":3,"alive":2}` |
| `/refresh` | POST | Force subscription re-fetch |

## Usage

### From Python (multi-instance)

```python
import requests

proxies = requests.get("http://localhost:27018/proxies").json()
proxy = proxies[0]  # Fastest proxy

r = requests.get("https://api.ipify.org", proxies={
    "http": f"http://{proxy['http']}",
    "https": f"socks5://{proxy['socks5']}",
})
print(r.text)
```

### From other Docker containers

```yaml
services:
  your-app:
    image: your-app
    environment:
      - HTTP_PROXY=http://v2prodock:27020
      - HTTPS_PROXY=socks5://v2prodock:27019
      - NO_PROXY=localhost,127.0.0.1,192.168.1.0/24
    networks:
      - proxy-net

networks:
  proxy-net:
    external: true
    name: v2prodock_v2prodock-proxy-net
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

1. Fetches subscription URL(s) and parses vless/vmess/trojan/shadowsocks links
2. Converts each to an xray-core JSON outbound config
3. Distributes configs across N instances (round-robin)
4. Each instance: starts xray, tests configs, keeps the first working one
5. Health checks run every 60s per instance — on failure, switches to next config
6. API returns alive proxies sorted by latency — dead ones excluded
7. Subscriptions re-fetched every 120s for updated server lists

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
| `SUBSCRIPTION_URL` | — | Single v2ray subscription URL |
| `SUBSCRIPTION_URLS` | — | Comma-separated URLs (one per instance, overrides `SUBSCRIPTION_URL`) |
| `PROXY_INSTANCES` | `1` | Number of xray instances to run |
| `PORT_BASE` | `27019` | Base port for proxy allocation |
| `API_PORT` | `27018` | Port for the HTTP API |
| `HEALTH_CHECK_URL` | `http://api.ipify.org` | URL used to test proxy connectivity |
| `XRAY_DIR` | `/root/xray` | Path to xray binary directory |
| `GOGC` | `100` | Go GC target percentage (lower = more frequent GC, less memory) |
| `GOMEMLIMIT` | `128MiB` | Go soft memory limit (prevents OOM by triggering aggressive GC) |
| `MAX_CONNS` | `128` | Max concurrent HTTP CONNECT relay connections |

### Tuning for Low-End Devices

For devices with limited RAM (256MB-512MB):

```bash
# .env — conservative defaults that won't OOM
GOGC=100
GOMEMLIMIT=128MiB
MAX_CONNS=64
PROXY_INSTANCES=1
```

```bash
# .env — aggressive for VPS with 1GB+ RAM
GOGC=50
GOMEMLIMIT=256MiB
MAX_CONNS=256
PROXY_INSTANCES=3
```

## License

MIT
