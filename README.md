# V2ProDock

<p align="center">
  <img src="https://github.com/user-attachments/assets/82685dd3-b43b-4e27-a7c8-02f3ea5edc67" alt="V2ProDock logo" width="150" height="150">
</p>

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
curl -fsSL https://raw.githubusercontent.com/411A/V2ProDock/main/install.sh | bash
```

Or clone manually:

```bash
git clone https://github.com/411A/V2ProDock.git && cd V2ProDock
[ -f .env ] || cp .env.example .env
# Edit .env with your subscription URL(s)
sudo bash install.sh
```

## Multi-Instance Setup

Run multiple independent proxy instances in a single container:

```bash
# .env
SUBSCRIPTION_URLS=https://sub1.example,https://sub2.example,https://sub3.example
PROXY_INSTANCES=3
```

Port layout: all SOCKS5 ports come first, then all HTTP ports. Instance `i` of `N` gets `SOCKS5=PORT_BASE+i` and `HTTP=PORT_BASE+N+i` (e.g. with `N=3`: SOCKS `27019-27021`, HTTP `27022-27024`). Query them via the API:

```bash
# Returns alive proxies sorted by lowest latency
curl http://localhost:27018/proxies
```

```json
[
  {"index":1, "socks5":"0.0.0.0:27020", "http":"0.0.0.0:27023", "status":"ok", "latency_ms":85, "name":"server-1"},
  {"index":0, "socks5":"0.0.0.0:27019", "http":"0.0.0.0:27022", "status":"ok", "latency_ms":120, "name":"server-2"}
]
```

### API Endpoints

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/proxies` | GET | Alive proxies sorted by latency (lowest first) |
| `/all` | GET | All instances including down ones |
| `/health` | GET | `{"status":"ok","instances":3,"alive":2,"starting":1}` |
| `/vpn` | GET | VPN pinning proof: upstream SOCKS + egress IP + `verified` |
| `/refresh` | POST | Force subscription re-fetch |

## VPN for legacy devices (IKEv2 + L2TP)

`TVs, IoT, consoles` connect to the `v2prodock-vpn` sidecar. Its traffic is
forced via `tun0` into the fastest **alive** Xray SOCKS — fail-closed, never
direct. Proofs land in the logs and in `GET /vpn`:

```bash
# .env
VPN_ENABLED=1
VPN_DOMAIN=192.168.1.10   # or public hostname/IP
VPN_USER=vpnuser
VPN_PASSWORD=change-me-8-chars-min
VPN_IPSEC_PSK=change-me-too-8-chars-min
```

```bash
curl http://localhost:27018/vpn
docker logs -f v2prodock-vpn   # look for: VPN egress VERIFIED via <name>: <ip>
```

### L2TP/IPsec — file-less login (recommended for legacy)

Type `L2TP/IPsec with pre-shared key`, server = `VPN_DOMAIN`, then enter the
IPsec PSK (`VPN_IPSEC_PSK`) + PPP username/password. No files to import —
works on old Windows/Android/routers natively.

### Bare L2TP — old stock firmware only (opt-in, insecure)

Some old routers (e.g. stock Asus with bare-L2TP client, no IPsec option)
cannot do IPsec at all. Set `VPN_ALLOW_PLAIN_L2TP=1` to accept them: server =
`VPN_DOMAIN`, no PSK, just PPP username/password. **Everything — login and
all traffic — crosses the internet in cleartext.** Default `0` refuses bare
L2TP at packet level. Use a unique strong password and only for devices that
cannot do IPsec.

### IKEv2 — needs one CA import

Server = `VPN_DOMAIN` (`UDP 500/4500`), `EAP-MSCHAPv2` with the same
username/password, after trusting `config/vpn/ca.crt`. Apple devices can use
`config/vpn/apple.mobileconfig` instead. Connect using exactly `VPN_DOMAIN`
(or a name/IP listed in `VPN_EXTRA_SANS`) — anything else fails server-identity
validation.

### Privileges & troubleshooting

The gateway needs `NET_ADMIN + MKNOD + SYS_ADMIN`, `/dev/net/tun`, and
`UDP 500/4500/1701` (see compose) — least privilege verified, no
`--privileged`. The host kernel needs PPP support (`modprobe ppp_generic`).
PPP diagnostics: `docker exec v2prodock-vpn cat /var/log/ppp.log`.
Single client stalled while others work: toggle VPN off/on on that device
(child-SA desync is DPD-blind; the server also recycles DATA SAs every
30 min regardless).
Client-side kill switch (essential): our gateway is fail-closed, but a
device whose own VPN SA dies will happily send traffic direct. On Android
enable “Block connections without VPN”, on Windows bind sensitive apps
with firewall rules to the VPN interface, on iOS use OnDemand mode. Without
this, no VPN provider can promise no-leak on the device itself.

### VM / VPS firewall & LAN clients

Docker publishes the ports, but a default-deny host firewall still blocks
them first. `install.sh` opens them automatically on `ufw`/`firewalld`
when `VPN_ENABLED=1`; otherwise do it by hand:

```bash
sudo ufw allow 500,4500,1701/udp
# or: sudo firewall-cmd --permanent --add-port={500,4500,1701}/udp && sudo firewall-cmd --reload
```

Cloud VMs need the same three UDP ports in the provider's security group.
IP protocol ESP (50) is **not** required anywhere — all IPsec is forced
through UDP/4500 encapsulation, which is also what makes same-LAN clients
work (raw ESP cannot cross Docker's port NAT into the container).

Example: project runs in a VM at `192.168.1.100`, legacy box on the same
LAN connects L2TP to server `192.168.1.100` with your user/pass (+ PSK for
L2TP/IPsec, none for opt-in bare L2TP). Set `VPN_DOMAIN=192.168.1.100` so
the server identity matches what clients dial. No host sysctls or forwarding
setup needed — the container handles its own networking.

WSL2 note: stock WSL2 is NAT mode (`172.x` private IP), so physical LAN
devices cannot reach services inside a WSL2 distro directly. This project
runs on Docker Desktop (ports published on the Windows host's own
interfaces, LAN-reachable), which we verified end-to-end from inside WSL2
(IKE handshake + L2TP control + generic UDP all answer). If you instead run
`dockerd` inside WSL2 itself, LAN devices need `netsh interface portproxy`
relays for UDP 500/4500/1701 from the Windows host into WSL2.

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
sudo bash install.sh           # Install & start
sudo bash install.sh start     # Start
sudo bash install.sh stop      # Stop
sudo bash install.sh status    # Show status
sudo bash install.sh logs      # Follow logs
sudo bash install.sh uninstall # Remove everything
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
| `SUBSCRIPTION_URL` | — | Single v2ray subscription URL (see [V2RayDAR](https://github.com/411A/V2RayDAR) to self-host one) |
| `SUBSCRIPTION_URLS` | — | Comma-separated URLs (one per instance, overrides `SUBSCRIPTION_URL`) |
| `PROXY_INSTANCES` | `1` | Number of xray instances to run |
| `PORT_BASE` | `27019` | Base port: N SOCKS5 ports, then N HTTP ports (`SOCKS=base+i`, `HTTP=base+N+i`) |
| `API_PORT` | `27018` | Port for the HTTP API |
| `HEALTH_CHECK_URL` | `http://api.ipify.org` | URL used to test proxy connectivity |
| `XRAY_DIR` | `/root/xray` | Path to xray binary directory |
| `GOGC` | `100` | Go GC target percentage (lower = more frequent GC, less memory) |
| `GOMEMLIMIT` | `128MiB` | Go soft memory limit (prevents OOM by triggering aggressive GC) |
| `MAX_CONNS` | `128` | Max concurrent HTTP CONNECT relay connections |
| `VPN_ENABLED` | `0` | `1` = serve IKEv2 + L2TP via working proxy |
| `VPN_DOMAIN` | `vpn.local` | Hostname/IP clients connect to (server cert SAN) |
| `VPN_SUBNET` | `10.10.10.0/24` | IKEv2 client pool (forwarded to tun0 only) |
| `VPN_L2TP_NET` | `10.10.11.0/24` | L2TP client pool (forwarded to tun0 only) |
| `VPN_DNS` | `1.1.1.1,8.8.8.8` | DNS pushed to clients (also via proxy) |
| `VPN_USER` / `VPN_PASSWORD` | — | EAP-MSCHAPv2 + PPP creds (or `VPN_USERS=u1:p1,u2:p2`) |
| `VPN_IPSEC_PSK` | — | IPsec pre-shared key for L2TP clients |

### Subscription URL

You need a v2ray subscription URL to get started. [V2RayDAR](https://github.com/411A/V2RayDAR) fetches configs from public repos and serves them as a single subscription endpoint — run it on your server and point to:

```
http://192.168.x.x:27141/subscription
```

> **Docker note:** Inside a container, `127.0.0.1` refers to the container itself, not your host. Use `host.docker.internal` (Docker 20.10+) or your host's LAN IP instead. The compose file already includes `extra_hosts` for `host.docker.internal` support on Linux.

> **WSL2 note:** If V2RayDAR runs on Windows and V2ProDock runs in WSL2/Docker, `host.docker.internal` won't reach the Windows host. The install script auto-detects WSL2 and replaces it with the correct Windows IP. To fix manually:
> ```bash
> WIN_HOST=$(ip route show default | awk '/default/ {print $3}')
> sed -i "s|host.docker.internal|$WIN_HOST|g" .env
> bash install.sh restart
> ```

You can also add more public subscription URLs directly via `SUBSCRIPTION_URLS` in `.env` — comma-separated, one per instance.

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
