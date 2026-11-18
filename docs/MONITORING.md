# Monitoring CPU on a burstable VPS

A 1 vCPU plan is usually **burstable**: it guarantees a fraction of one core
(for example 10%) and lets you exceed it briefly. When sustained usage stays
above the guarantee, the provider throttles the whole server. The warning is
about the **VM**, not about one container being misconfigured.

## The one number that matters

`docker stats` reports percentages of **one full core**. On a 1 vCPU server:

| `docker stats` shows | Meaning |
|---|---|
| `100%` | one entire core |
| `10%` | the whole guaranteed allowance on a 10%-of-a-core plan |
| `183%` | almost two cores — sustained usage that will be throttled |

Aim for **sustained total below roughly 10%**, leaving room for the OS, the
Docker daemon, and the kernel's own work. Short spikes are normally fine; the
warning comes from the average.

## Quick snapshot (all containers)

```bash
docker stats --no-stream --format 'table {{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}'
```

Per-container history, cheap enough to leave on:

```bash
sudo apt install -y sysstat
(crontab -l 2>/dev/null; \
 echo '*/5 * * * * docker stats --no-stream --format "{{.Name}} {{.CPUPerc}} {{.MemUsage}}" >> /var/log/docker-stats.log') \
 | crontab -
```

Then:

```bash
sar -u            # whole-VM CPU history (usr + sys + iowait)
sar -u 1 5        # five one-second samples right now
cat /var/log/docker-stats.log
```

`sysstat` keeps a rolling history, so it can show the spike that a single
`top` snapshot missed.

## Per-process attribution

`docker stats` bundles a container's children. To see what is inside:

```bash
docker exec v2prodock top -b -n1 | head -25
docker exec v2prodock-vpn top -b -n1 | head -25
```

Inside `v2prodock`, the processes are the Go daemon and one `xray` child per
`PROXY_INSTANCES`. The xray children carry the actual proxied traffic; the Go
daemon handles subscriptions, health checks, and the SOCKS/HTTP bridges.

## What to expect from this stack

- **`v2prodock`, proxy-only:** the daemon is small (roughly 15–17 MB RSS on a
  loopback test rig) and idles at a fraction of a percent. The xray child is
  the data plane and is idle when no client is transferring.
- **`v2prodock-vpn` with `VPN_ENABLED=0`:** deliberately idle. It sleeps and
  its healthcheck now short-circuits, so it should not wake for `swanctl`.
- **`v2prodock-vpn` with `VPN_ENABLED=1`:** the gateway proves egress through
  the tunnel every `POLL_SECS` (default 15s). That means two small HTTPS
  requests per poll **by design** — this is the one component whose idle cost
  is intentionally non-zero. If you do not use the VPN sidecar, leave
  `VPN_ENABLED=0`.

## "It connects, then large transfers stall" — quota or bad hop?

These are different failures and need different fixes:

| Symptom | Likely cause |
|---|---|
| CPU% sits at/above the guarantee, `nr_throttled` keeps rising, stalls are variable and affect several flows | Host CPU quota throttling |
| A repeatable ceiling at the same byte count (~12–16 KB) on one proxy, while another proxy moves the same file | Broken/truncating upstream hop |
| Large TLS transfers stall but tiny requests work, on every path | Path MTU / MSS (especially VPN clients) |
| 204 health checks pass but bulk throughput is 1–7 KB/s on one node | Shaped/throttled upstream; the bulk gate demotes it (`throughput_kbps`, `degraded`) |

`XRAY_FRAGMENT=1` is an anti-RST tool, not a speed setting. Measured on a
shaped link, the split ClientHello was itself the fingerprint: identical
ports went from full-speed to 1–7 KB/s with fragment on. Leave it `0` unless
handshakes are actually being reset, and watch `throughput_kbps` after
enabling it. The gate demotes a node that collapses, but it cannot undo the
shaping for a pool that was already working.

Check the container's cgroup throttle counters **during** playback:

```bash
docker exec v2prodock cat /sys/fs/cgroup/cpu.stat 2>/dev/null || \
  docker exec v2prodock cat /sys/fs/cgroup/cpu/cpu.stat
```

`nr_throttled` and `throttled_usec` are cumulative. If they grow every few
seconds while streaming, the host quota — not this code — is the limit. A
10%-of-a-core guarantee is 100 ms of CPU per second; TLS + proxy crypto for
sustained 4K video can exceed that even though connecting and small requests
are fine.

For the truncation test (bytes and checksums, not `HEAD`) and the port layout
that identifies whether the failing port is even ours, see the README section
[When a transfer stops at ~12 KB](../README.md#when-a-transfer-stops-at-12-kb).

## If throttling happens again

Capture the evidence the provider asks for **during** the spike, with a
timestamp and timezone:

```bash
date -Is
docker stats --no-stream --format 'table {{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}'
top -b -n1 | head -25
sar -u 1 5
```

Send the output plus the container names. On a shared VPS the busiest
container may not be this project at all; the numbers make that visible.

## Do not "fix" it with a low cgroup CPU limit

The compose file's `deploy.resources.limits.cpus` is a **container** limit; it
does not raise or replace the provider's **host** quota. Setting it to `0.1`
does not buy more headroom — it throttles this container and can reduce
throughput. Keep the limit as a safety cap (the default `0.5` is fine), and
reduce actual work instead: fix busy loops, lower polling frequency where the
feature allows it, and keep idle paths idle.
