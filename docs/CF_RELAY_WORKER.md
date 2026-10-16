# CF relay egress — worker contract + deployment options

## Do this, in this order (the 2-minute version)

- **Step 0 — nothing is broken.** The feature ships OFF. With no
  `CF_RELAY_URL` set, V2ProDock behaves exactly as before. Do the steps
  below only when you want the clean-IP path; there is no rush.
- **Step 1 — pick A or B.** Your current worker speaks only the custom
  `x-relay-target` header, which V2ProDock cannot use (see contract below).
  Either **A:** teach that worker standard `CONNECT` (keeps your Cloudflare
  egress IP), or **B:** run a tiny standard proxy on any host with a clean
  IP (works today, no worker change).
- **Step 2 — prove the relay speaks CONNECT, BEFORE touching V2ProDock.**
  From any machine with curl:
  ```bash
  curl -sS --max-time 15 -x https://YOU.workers.dev:443 https://api.ipify.org/
  # want: a Cloudflare IP printed. "400" or a hang = not ready, stop here.
  ```
  With user/pass: `curl -x https://user:pass@YOU.workers.dev:443 ...`
- **Step 3 — wire the daemon** (on the V2ProDock host, file `.env`):
  ```env
  CF_RELAY_URL=https://YOU.workers.dev
  AGGREGATE_RELAY_PORT=27015
  ```
  ```bash
  cd /home/agent/V2ProDock && docker compose up -d
  curl -s http://127.0.0.1:27018/health   # want "aggregate_relay" present
  docker logs v2prodock 2>&1 | grep -i relay   # want "Relay egress armed"
  ```
- **Step 4 — prove end-to-end through the daemon:**
  ```bash
  curl -sS --max-time 20 -x socks5h://127.0.0.1:27015 https://api.ipify.org/
  # want: the SAME Cloudflare IP as step 2 (tunnel in, clean IP out)
  ```
- **Step 5 — use it, cautiously.** Point non-critical clients at
  `socks5h://127.0.0.1:27015`. Watch one health tick for
  `relay egress alive` in the logs. Keep Hermes on `:27016`/`:27017`
  until then. Rollback = comment out the two lines, `docker compose up -d`.

## How the path works

Full chain: client → relay aggregate (`:27015`) → per-instance relay
inbound → `relay-out` HTTP outbound → **inside the
working upstream tunnel** → relay → target. The censor sees only the
upstream transport; the target sees the relay's egress IP.

## The contract (non-negotiable)

The relay MUST be a standard HTTP proxy:

- `CONNECT host:port HTTP/1.1` → `200 Connection Established`, then raw bytes.
- Optional Basic auth (`Proxy-Authorization` / `407 Proxy Authenticate`).
- `https://` relay URLs: TLS to the relay with a publicly-trusted cert
  (the daemon verifies; there is deliberately no skip knob).

Why: xray's `http` outbound only speaks this protocol, and the aggregate is
a transparent TCP relay into an end-to-end TLS stream — there is nowhere to
inject a custom header. Verified 2026-10-17: a header-only relay answers
`400` to both plain `GET /` (`Missing x-relay-target header`) and `CONNECT`,
so it can never sit behind `relay-out`.

## Option A — teach your Worker CONNECT (keeps your egress)

Reference sketch (adapt to your plan's sockets availability; the PROTOCOL
above is what's tested, not this listing):

```js
import { connect } from "cloudflare:sockets";

const USER = "relay"; // prefer a Worker secret
const PASS = "change-me";

export default {
  async fetch(req) {
    if (req.method !== "CONNECT") {
      return new Response("relay: send CONNECT host:port", { status: 400 });
    }
    const auth = req.headers.get("proxy-authorization") || "";
    if (auth !== "Basic " + btoa(USER + ":" + PASS)) {
      return new Response("proxy auth required", { status: 407,
        headers: { "proxy-authenticate": 'Basic realm="relay"' } });
    }
    const authority = req.headers.get("host") || "";
    const [hostname, portStr] = authority.split(":");
    const socket = connect({ hostname, port: Number(portStr) || 443 });
    await socket.opened;
    req.body?.pipeTo(socket.writable).catch(() => {});
    return new Response(socket.readable, { status: 200 });
  },
};
```

Verify with Step 2 above (direct CONNECT against the worker), then continue
at Step 3. Extra checks once wired:

```bash
curl -sS --max-time 20 -x socks5h://127.0.0.1:27015 https://api.ipify.org/
curl -sS --max-time 20 -o /dev/null -w "%{http_code}\n" \
  -x socks5h://127.0.0.1:27015 https://api.telegram.org/
```

## Option B — standard proxy on a clean VPS (works today, no worker change)

Any `CONNECT`-speaking proxy on a host with a clean IP (tinyproxy, squid,
or a 20-line Go CONNECT forwarder). It is reached THROUGH the tunnel, so
DPI still sees only VLESS:

```bash
# on the clean host
sudo apt install -y tinyproxy   # /etc/tinyproxy/tinyproxy.conf: Port 8888, Allow 0.0.0.0/0 + BasicAuth
```

```env
CF_RELAY_URL=http://user:pass@clean.host:8888
```

## Wiring the daemon

```env
CF_RELAY_URL=https://YOU.workers.dev
AGGREGATE_RELAY_PORT=27015
```

```bash
docker compose up -d
curl -s http://127.0.0.1:27018/health  # aggregate_relay appears when armed
```

Point Hermes (or anything else) at `socks5h://127.0.0.1:27015`.
While the relay path is down the port fails fast (refused at once) and
`:27016`/`:27017` keep serving — keep critical traffic on those until the
relay has proven itself for a few ticks (watch for `relay egress alive`).

## What it does NOT do

- No Telegram benefit by default: the Bot API doesn't filter on client-IP
  reputation. Use this path for destinations that actually IP-ban, or keep
  everything on the direct aggregates.
- No fallback INTO the relay: if the tunnel is down, the relay is
  unreachable by construction (it is dialled through the tunnel).
- No header-protocol support, ever: anything xray can't speak would fail
  at every render instead of cleanly at startup, so `CF_RELAY_URL` only
  accepts `http(s)://` and anything else disables the feature with a warning.
