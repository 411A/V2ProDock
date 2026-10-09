# install.sh end-to-end validation findings

Validation run: 2026-10-09. HEAD `2a466bf` (`2a466bf59f42a93d799829aea6a1bc064ba2b774`), tree
clean before and after. Nothing in the repo was modified — `install.sh` was verified
byte-identical to HEAD via `git hash-object install.sh` vs `git rev-parse HEAD:install.sh`
(both `0968f235a288d46b9e5bb8e40fee9e6a9de4af69`). Nothing was committed.

Everything below was executed. Claims that are reasoned but not demonstrated are labelled
**hypothesis**.

---

## 1. What `install.sh` does, in order

`install.sh` is 885 lines, `set -uo pipefail` (note: **no `set -e`**), bash-only. It has no
dry-run, no `--check`, no `--yes`, no non-interactive flag — I grepped for all of them and
for `read -r -p` / `read -r -s -p` (11 interactive prompts at lines 670, 710, 737, 740, 743,
746, 748, 750, 752, 762, 844, 855). The only non-interactive forms are the subcommands and
piping answers on stdin, which is what I used throughout.

### Preamble (lines 1–109) — HOST-MUTATING

| # | Operation | Host mutation | Guarded | Idempotent |
|---|---|---|---|---|
| 0 | `git clone`/`git pull`/`git reset --hard` into `$HOME/V2ProDock` (93–109) | **Yes** — writes `$HOME/V2ProDock`; on ff-failure does `git stash --include-untracked` + `git reset --hard origin/main` | only when **not** inside a checkout (`! -f v2proxy/main.go \|\| ! -f docker-compose.yml`) | yes |
| 1 | mode detect (633) | no | `docker && docker compose version` | yes |

### Install / update path (`*)`, lines 677–792 — HOST-MUTATING

| # | Operation | Line | Host mutation | Guarded | Idempotent | How verified |
|---|---|---|---|---|---|---|
| 2 | `git pull --ff-only`; on failure `git stash --include-untracked` → `fetch` + `reset --hard origin/main` → `stash pop` | 681–703 | **Yes** — rewrites the checkout | `if [ -d "$DIR/.git" ]` | yes | read; not exercised (my `/work` had no `.git`) |
| 3 | `mkdir -p "$DIR/config"` | 704 | Yes (repo) | no | yes | observed created |
| 4 | prompt for subscription URL; write `config/subscription.txt` | 708–715 | Yes | only if no source found | yes | observed written |
| 5 | `rewrite_sub_urls` — WSL2 / VM loopback rewrite | 718 | no | only for URLs containing `127.0.0.1`/`localhost`/`host.docker.internal` | yes | **observed — see F1** |
| 6 | `check_subscriptions_reachable` — `curl -sf` per URL | 722 | no | warn-only | yes | observed `[OK] 1/1 reachable` |
| 7 | **create `.env`** from `.env.example`, then `write_env_subscriptions`, then 6 interactive config prompts | 729–771 | **Yes** — writes credentials to disk (mode 0600, confirmed) | only `if [ ! -f "$DIR/.env" ]` | yes | observed created |
| 8 | `ensure_host_prereqs` | 787 → 517 | **YES — the only true host-mutating step** (see below) | see below | yes | observed, see §2 |
| 9 | `fresh_rebuild` | 788 → 367 | **Yes** — full teardown + image delete + rebuild | no | yes (always rebuilds) | observed |
| 10 | `ok "Started"` | 789 | no | **unconditional — see F2** | — | observed |
| 11 | `check_container_egress` | 790 → 293 | no | warn-only | yes | observed `[OK] In-container reachable` |
| 12 | `show_status` | 791 → 23 | no | no | yes | observed |

### `ensure_host_prereqs` (517–628) — the host-mutating core

| Operation | Line | Mutation | Guard |
|---|---|---|---|
| `modprobe tun ppp_generic ppp_async ppp_mppe xt_policy nf_conntrack_pptp nf_nat_pptp` | 527–537 | **kernel module load** | root, or `sudo -n` only (never prompts). Never runs as plain user. |
| write `/etc/modules-load.d/v2prodock.conf` | 541–545 | **Yes, persists across reboot** | root or `sudo -n` |
| `/dev/net/tun` presence check | 547 | no | warn only |
| `ufw allow 500/udp 4500/udp 1701/udp` | 577 | **YES — firewall rules, permanent** | **only if `VPN_ENABLED=1`** (551) + root/`sudo -n` |
| `ufw allow … port 1723 proto tcp` + `ufw allow proto gre` from RFC1918 | 579–584 | **YES** | only if `VPN_ENABLE_PPTP=1` + VPN_ENABLED=1 |
| `firewall-cmd --permanent --add-port` / `--reload` | 598–601 | **YES** | only if VPN_ENABLED=1 + firewalld active + root |
| `lsmod \| grep nf_conntrack_pptp` warn | 621–627 | no | only if PPTP on |

So the firewall mutation is correctly double-gated on `VPN_ENABLED=1` — a proxy-only install
touches no firewall rules. `modprobe` + the modules-load.d file are **not** gated on VPN and
run on every install.

### `fresh_rebuild` (367–419) — HOST-MUTATING

| Operation | Line | Mutation |
|---|---|---|
| `docker compose down --remove-orphans` | 369 | removes project containers + network |
| `docker compose ps -aq` → `docker rm -f` survivors | 372–378 | force-removes stuck containers |
| `docker rmi v2prodock/proxy:latest v2prodock/vpn-gateway:latest` | 385 | deletes both project images |
| `docker compose build` | 386 | **builds every service**, hard-fails on any |
| `docker compose up -d` + `verify_project_containers`, up to 3 attempts | 386–414 | creates containers, publishes 27000–27100 + 500/4500/1701 udp + 1723 tcp |
| `free_port_squatter` | 426–460 | may `kill -9` a docker-proxy — **only** when the container ID is gone. Refuses live holders and non-proxy processes. Correctly scoped. |

Scope discipline is good: no `prune`, no volume removal, no system-wide flags, explicit image
names. `uninstall` (669–676) prompts `y/N`, then `compose down -v`, `docker rmi` the two images,
`rm -rf config .env`.

### Subcommands

- `start` (644–650): `ensure_host_prereqs` + `fresh_rebuild` + egress check + status.
  **Never creates `.env`** — see F3.
- `stop` (651–654): `docker compose stop` only. No verification (see F4). Idempotent, verified.
- `status` (655–665): `compose ps` + `verify_project_containers` + logs + `/vpn`.
- `logs` (666–668), `uninstall` (669–676): both verified working.
- Anything else falls into `*)` = full install. **`restart` is not an arm** — see F9.

---

## 2. Genuine end-to-end run

Docker on the host is Docker Desktop, Linux containers, `27.5.1`, compose `v2.32.4-desktop.1`.

**Containment.** I never ran `install.sh` on Windows or on the Ubuntu WSL distro. I ran it
inside a disposable Docker-in-Docker container with the repo bind-mounted in:

```
docker run -d --name v2e2e-probe --privileged \
  -v "<temp>\v2e2e\repo:/work" -w /work \
  docker:27-dind --host unix:///var/run/docker.sock
docker exec v2e2e-probe apk add --no-cache bash curl iproute2
docker exec v2e2e-probe bash /work/_run.sh > /work/install-run4.log 2>&1
```

Every `docker compose` call, every container, every published port lived inside the dind
netns. Nothing reached the Windows host or the Ubuntu WSL distro. The harness was deleted and
the dind container removed at the end (`docker ps -a` empty).

Real public subscription used throughout:
`https://raw.githubusercontent.com/0xRadikal/Free-v2ray-Configs/main/top100.txt` (20,554 bytes,
HTTP 200). No VPN peer, no external VPN server. The one VPN password typed was the literal
throwaway string `throwaway-test-only` inside the sandbox; the IPsec PSK was auto-generated
by `openssl rand`. No real credential was ever invented or entered.

### Result: **install completed, EXITCODE=0**

```
[OK] Docker detected
[OK] 1/1 subscription URL(s) reachable
[OK] Using 1 subscription source(s)
[OK] .env created from .env.example
 v2prodock  Built
 v2prodock-vpn  Built
 Network work_v2prodock-proxy-net  Created
 Container v2prodock  Started
 Container v2prodock-vpn  Started
  v2prodock: running (restart=unless-stopped)
  v2prodock-vpn: running (restart=unless-stopped)
[OK] Started
[OK] In-container reachable: https://.../top100.txt
... show_status output ...
EXITCODE=0
```

Post-install state and real traffic through the aggregate:

```
/v2prodock|running|unless-stopped|healthy
/v2prodock-vpn|running|unless-stopped|unhealthy

direct egress            : 187.13.208.194
aggregate SOCKS 27017    : 51.81.203.63
per-instance SOCKS 27019 : 51.81.203.63
aggregate HTTP  27016    : 51.81.203.63
per-instance HTTP 27020  : 51.81.203.63
/health: {"aggregate_http":"0.0.0.0:27016","aggregate_socks":"0.0.0.0:27017","alive":1,...}
```

All four proxy paths serve real traffic; egress IP differs from direct.

### Two harness deviations (both environmental, both disclosed)

1. **`vpn-gateway/Dockerfile` could not build here.** Its step 2 fetches pptpd from a
   SourceForge `/download` redirect, and in this sandbox the redirect target
   (`gigenet.dl.sourceforge.net` / `pilotfiber.dl.sourceforge.net`) does not resolve for
   `wget`, while `curl -sSL` to the same URL returns HTTP 200 / 252,167 bytes. Confirmed at
   container level, independent of install.sh. I patched only the **copy inside the
   disposable container** (`wget` → `curl -sSL`) to get past it. The repo file is untouched.
2. Because `install.sh:386` runs a bare `docker compose build` (all services), the default
   `VPN_ENABLED=0` install **cannot** succeed if the VPN sidecar image fails to build — the
   proxy-only user is blocked by a service they disabled. That is finding **F5**.

---

## 3. Structural claims — verified

| Claim | Verdict | Evidence |
|---|---|---|
| Container names match compose | **TRUE** | `install.sh:326` `PROJECT_CONTAINERS="v2prodock v2prodock-vpn"` vs `docker-compose.yml:5` `container_name: v2prodock` and `:80` `container_name: v2prodock-vpn`. `docker compose config` shows exactly those two `container_name` values; no drift. |
| `restart: unless-stopped` on both | **TRUE** | `docker-compose.yml:6` and `:82`. Confirmed at runtime: both reported `restart=unless-stopped`, and `verify_project_containers` printed no warning. |
| Project-scoped image namespace | **TRUE** | `docker-compose.yml:4` `v2prodock/proxy:latest`, `:79` `v2prodock/vpn-gateway:latest`. `install.sh:385` and `:673` `docker rmi` exactly those two tags — no glob, no `prune`. |
| `PROJECT_CONTAINERS` not drifted from compose | **TRUE (for container names)** | See above. **Caveat:** the *network* is project-scoped by directory name, not by `PROJECT_CONTAINERS` — from `/work` it was `work_v2prodock-proxy-net`, from `/work2` it was `work2_v2prodock-proxy-net`. The comment at `install.sh:321–325` calls the two names "the project's whole footprint"; the network is a third artifact not in that list. See F8. |
| `docker-compose.host.yml` is an override only | **TRUE** | `docker compose config` from the repo root does not pick it up (verified: `network_mode: host` count = 0). `README.md:294,355` document `-f docker-compose.yml -f docker-compose.host.yml`. |

---

## 4. Defects and risks, ranked

### F1 — HIGH — `[OK]` progress message is captured into the subscription URL and written to `.env`

`install.sh:218–220`. Inside `rewrite_sub_urls`, `ok "Rewriting …"` writes to **stdout**, and
the function's stdout is what the caller captures (`fixed_urls=$(rewrite_sub_urls …)` at line
718). The `[OK]` line becomes part of the URL list.

Demonstrated. Feeding `http://127.0.0.1:27141/subscription`:

```
[OK] Rewriting http://127.0.0.1:27141/subscription -> http://172.17.0.1:27141/subscription
```

and in the resulting `.env`:

```
SUBSCRIPTION_URL=[0;32m[OK] Rewriting http://127.0.0.1:27141/subscription -> http://172.17.0.1:27141/subscription[0m
```

and in the container's live environment:

```
SUBSCRIPTION_URL=<ESC>[0;32m[OK] Rewriting http://127.0.0.1:27141/subscription -> http://172.17.0.1:27141/subscription<ESC>[0m
```

That URL is unusable. The app then reports, four times per restart loop:

```
ERRO scan failed: EOF
Enter subscription URL: Subscription URL is required
```

This fires for **any** user whose subscription URL contains `127.0.0.1`, `localhost`, or
`host.docker.internal` — i.e. precisely the documented local-subscriber setups in
`.env.example:6–12`. It requires no WSL: I confirmed `fix_vm_url` alone (`install.sh:498–512`)
is enough, via its `get_host_gateway_ip` fallback. The whole install still exits **0**.

Note the diagnostic ordering is also corrupted: `check_subscriptions_reachable` printed
`[WARN] Cannot reach [OK] Rewriting … from host` — the progress line landed inside another
message.

### F2 — HIGH — `install.sh start` reports "Started" and exits 0 while the proxy container is in a crash-restart loop

This is exactly the failure class `install.sh:328–337` claims to catch. It catches the
*state* but not the *outcome*, and nothing propagates it into an exit code.

Demonstrated with a `COMPOSE_FILE` override that makes `v2prodock`'s entrypoint `exit 42`:

```
 Container v2prodock  Started
[OK] Started
[WARN] v2prodock is 'restarting', not running
[OK] Started                            <-- install.sh:789, unconditional
[WARN] v2prodock container is not running - cannot verify in-container egress
START_EXITCODE=0
daemon: v2prodock | Restarting (42) Less than a second ago
        RestartCount=5
```

A user watching this sees `[OK] Started` twice and a green `[OK]`-laden transcript, gets a
working-looking install, and has a container that will never stay up. `docker compose up -d`
returned 0 here because compose only reports create/start success, not survival.

The same shape occurs with **no override at all**: `install.sh start` on a checkout with no
`.env` (F3) produced `RestartCount=5`, `ExitCode=0`, `SUBSCRIPTION_URL=` empty, and
`[OK] Started` / `START_EXITCODE=0`.

`verify_project_containers` (`install.sh:338–353`) is read-only by design and correct in what
it reports; the defect is that its warnings are the *only* signal and `fresh_rebuild:394`
returns 0 unconditionally after calling it.

### F3 — MEDIUM-HIGH — `install.sh start` on a checkout with no `.env` produces a permanently dead proxy

`install.sh:644–650`. The `start` arm skips the entire config block (729–785). It never
creates `.env`, so compose passes `SUBSCRIPTION_URL=` empty, and `v2proxy/main.go:73–82`
prompts on stdin, gets EOF, logs `Subscription URL is required` and `os.Exit(1)`.

Demonstrated:

```
START_EXITCODE=0
.env created by the start path?  NO - start never creates .env
SUBSCRIPTION_URL=           (empty)
daemon: v2prodock | Restarting (1) 1 second ago   RestartCount=5
health: (no response)
```

The realistic trigger is a user who ran `install.sh uninstall` (which deletes `.env` at line
674) and then `install.sh start`, or who clones fresh and reads the README's `install.sh start`
line (README.md:426) as "start the already-installed service". `restart: unless-stopped` means
this crash-loops forever rather than settling.

### F4 — MEDIUM — `install.sh stop` verifies nothing, and the `Created`-state half-install is left behind

Two related gaps.

**(a)** `install.sh:652–654` is `docker compose stop; ok "Stopped"`. `ok` is unconditional.
A container that was already `Created` (never started) or in a restart loop is not stopped at
all, and the user is told "Stopped". Verified idempotent when things are healthy (ran twice,
both exit 0), but the success message is unearned in the failure cases.

**(b)** When `compose up` fails mid-start, `fresh_rebuild` exits at `install.sh:418` **without
`compose down`**, leaving the project in a half-installed state. Observed after a deliberate
port conflict:

```
[ERROR] docker compose up failed - fix the error above, then retry
v2prodock     | Created
v2prodock-vpn | Created
work_v2prodock-proxy-net   (network left behind)
```

Both containers exist but were never started. The comment at `install.sh:410–411` shows the
author knew about this — it `compose down`s before *retrying*, but never on the final
give-up path.

### F5 — MEDIUM — the port-squatter eviction path is dead code against Compose v2

`install.sh:398`:

```bash
bind_spec=$(printf '%s' "$up_out" | grep -oE 'failed to bind host port [0-9.]+:[0-9]+/(tcp|udp)' …)
```

`failed to bind host port X:Y/tcp` is the **docker-compose v1 (Python)** error string. Compose
v2 (both `v2.32.4` here and `v2.33.0` in the dind) emits:

```
Error response from daemon: driver failed programming external connectivity on endpoint
v2prodock (…): Bind for 127.0.0.1:27018 failed: port is already allocated
```

Demonstrated: I squatted `127.0.0.1:27018` with a live container and ran the real installer.

```
compose_rc=1
install.sh:398 regex against the real message -> no match (rc=1)
grep -oiE 'Bind for [^ ]+ failed: port is already allocated' -> matches
```

Because `bind_spec` is empty, line 399 `[ -z "$bind_spec" ] || …` breaks out of the retry loop
immediately. `free_port_squatter` (426–460) is **never reached from the installer**, so the
automatic stale-`docker-proxy` eviction documented in the comments at 355–366 never fires.
`install.sh start` then exits 1 with only the generic "If 'address already in use' persists"
hint — pushing the manual `ss -tlnp` step back onto the user that the code claims to automate.

`free_port_squatter` itself is well-written (refuses live holders, refuses non-proxy
processes, uses `ps ww` to avoid truncated-cmdline false negatives). It is simply unreachable.

### F6 — MEDIUM — `.env` is rewritten on every single run when more than one subscription URL is configured

`install.sh:774–781` compares `join_commas "$(read_env_sub_urls)"` against
`join_commas "$sub_urls"`. The two readers use **opposite orderings**:

- `read_sub_urls` (185–201): `config/subscription.txt` → `SUBSCRIPTION_URLS` → `SUBSCRIPTION_URL`
- `read_env_sub_urls` (175–182): `SUBSCRIPTION_URLS` → `SUBSCRIPTION_URL`

With two URLs `U1,U2`, the installer persists `SUBSCRIPTION_URL=U1` + `SUBSCRIPTION_URLS="U2"`,
and `config/subscription.txt` holds `U1,U2`. So `read_sub_urls` → `U1,U2` but
`read_env_sub_urls` → `U2,U1`. The comparison can never be equal.

Demonstrated:

```
pass 1 (U1,U2): .env written
pass 2 (U1,U2): [OK] Updated subscriptions in .env     <-- should say "up to date"
                 .env byte-identical afterwards (md5 7fadb405…)
```

So it is *content*-idempotent (the rewrite is a no-op) but reports the wrong thing every
time and does a pointless `mktemp` + `mv` + rewrite on every invocation. With a single URL the
two orders coincide and the message is correct. The fix is to sort both sides or compare
order-insensitively.

### F7 — MEDIUM — three `[OK]` messages that are printed unconditionally after failures in the non-Docker path

Reached whenever `docker compose version` fails, including the common "Docker installed but
the compose plugin is not" state — `install.sh:633` reports that as **"Docker not found,
installing dependencies…"** and drops into the legacy non-Docker branch.

Demonstrated with a shim where `docker` exists but `docker compose` errors:

```
DOCKER_MODE=false  <- docker EXISTS (/tmp/fakebin/docker) but compose is missing
Docker not found, installing dependencies...
install.sh: line 798: sudo: command not found
[OK] Go installed                 <-- /usr/local/go exists? NO
install.sh: line 808: sudo: command not found
chmod: /usr/local/bin/zellij: No such file or directory
[OK] Zellij installed             <-- /usr/local/bin/zellij exists? NO
install.sh: line 832: go: command not found
[OK] Built                        <-- nothing was built
```

Lines 798–801, 806–811 and 829–835 each print `ok` with no check of the preceding command's
status, and `set -e` is absent. Three false successes, then the user is told "Config ready".

### F8 — MEDIUM — two checkouts of the same repo collide, and the conflict is misreported

`container_name:` is a fixed global name (`docker-compose.yml:5,80`), but the compose project
and network are derived from the directory. A second checkout gets a different project name
and therefore a different network, but wants the same container names.

Demonstrated: `cp -a /work /work2` then `install.sh start` from `/work2`:

```
project name from /work : work
project name from /work2: work2
Error response from daemon: Conflict. The container name "/v2prodock" is already in use by
container "e85bc9e0…". You have to remove (or rename) that container to be able to reuse that name.
[ERROR] docker compose up failed - fix the error above, then retry
  If 'address already in use' persists: ss -tlnp | grep <port> finds the squatter
```

`fresh_rebuild` only evicts containers its own `compose ps -aq` reports (project `work2`), so
it cannot see or evict the `work` project's containers — correctly, per its scoping comment.
But the recovery hint printed is about **ports**, and there is no port problem. The operator is
sent to `ss -tlnp` for a name conflict. Also note `work2_v2prodock-proxy-net` was left behind.

The `PROJECT_CONTAINERS` list is also global: `verify_project_containers` will report on a
container belonging to a *different* checkout, which contradicts the "this is the project's
whole footprint" framing at `install.sh:321–325`.

### F9 — MEDIUM — `README.md:553` documents `bash install.sh restart`, which does not exist

`install.sh` has arms for `start`, `stop`, `status`, `logs`, `uninstall` only. There is no
`restart`, so `restart` falls through to `*)` = the **full install path**, which runs
`git pull`, `.env` rewrite, and `fresh_rebuild` (teardown + `docker rmi` both images + rebuild
+ `up`). Demonstrated:

```
$ bash install.sh restart
[OK] .env subscriptions up to date
[OK] Started
[OK] In-container reachable: https://.../top100.txt
restart_rc=0
```

The documented recovery step for the WSL2 URL-rewrite problem is therefore a full rebuild with
both images deleted, not a restart. Exit code 0, so nothing looks wrong.

### F10 — MEDIUM — `netns_report`'s "EXTRA COMPOSE FILE" hint fires on a file compose never loads

`install.sh:272`:

```bash
ls docker-compose*.yml compose*.yml 2>/dev/null | grep -vx 'docker-compose.yml' | sed 's/^/  host    EXTRA COMPOSE FILE: /'
```

This lists *any* matching filename. In a normal checkout it always flags
`docker-compose.host.yml`, which compose does **not** auto-load (verified: `docker compose
config` shows `network_mode: host` count = 0; `docker compose ls` lists only
`/work/docker-compose.yml`). The diagnostic that is supposed to point at the real cause of a
missing default route therefore names an innocent file in every single invocation, diluting
the one signal that matters (`internal=true` in the actual network config).

### F11 — LOW-MEDIUM — `show_status` prints ports that compose never published when `PROXY_INSTANCES > 41`

`docker-compose.yml:18–19` publishes `27000-27017` and `27019-27100`; `27018` is loopback-only.
With `PORT_BASE=27019` and `N` instances, `show_status` (`install.sh:31–37`) computes HTTP leg
`PORT_BASE+N … PORT_BASE+2N-1`. That overruns the published range at `N ≥ 42`.

Demonstrated with `PROXY_INSTANCES=42`:

```
PROXY_INSTANCES=41 -> http 27060..27100  [fits]
PROXY_INSTANCES=42 -> http 27061..27102  [OVERRUNS the published range]

health: {"alive":42,"instances":42,"starting":0,"status":"ok"}   <-- all healthy in-container
curl -x http://localhost:27100 ...  -> 103.11.76.248     (published, works)
curl -x http://localhost:27061 ...  -> 173.244.56.6      (published, works)
in-container 127.0.0.1:27101         -> 15.204.97.214    (listening, unreachable from host)
host localhost:27101                 -> rc=7 (connection refused)
```

`/health` reports `alive:42` and `status:ok` while two of the 42 HTTP legs are unreachable from
the host. `show_status` printed all 42 port pairs with no warning and no bound check. Health
is genuinely green; only the printed port list lies. **Hypothesis** (not demonstrated): the
aggregate HTTP listener would still serve, since it binds `27016`.

### F12 — LOW — the VPN sidecar is permanently `unhealthy` in the default proxy-only mode

`docker-compose.yml:138–143` sets the vpn healthcheck to
`swanctl --list-sas || swanctl --list-conns | grep -q ikev2-eap`. With `VPN_ENABLED=0` the
entrypoint idles (`VPN_ENABLED=0: v2prodock-vpn idle (proxy-only mode)`) and strongSwan has no
SAs, so the probe can never pass. Observed with `VPN_ENABLED=0` (the default):

```
/v2prodock-vpn|running|unless-stopped|unhealthy
healthcheck_rc=1
```

`install.sh status` reports `v2prodock-vpn: running (restart=unless-stopped)` with **no**
warning — `verify_project_containers` checks state and restart policy, not health. So on every
default install, `docker ps` shows an unhealthy container that the installer declares fine.
It is cosmetic (nothing routes to it) but it trains users to ignore the health column, which
matters for the case where VPN *is* enabled. With `VPN_ENABLED=1` the same container reported
`healthy`.

### F13 — LOW — `install.sh stop` leaves `restart: unless-stopped` in place but the compose project intact, so a later `docker compose up` resurrects the same containers

Not a defect per se; noted because `stop` and `uninstall` leave materially different states
(`stop` keeps the network and images, `uninstall` removes `config` and `.env`) and neither is
distinguished in the summary output beyond the single word "Stopped" / "Removed".

---

## 5. The vpn-gateway sidecar — what was exercisable without a real VPN peer

**Yes, substantially.** `VPN_ENABLED=1` needs no external VPN server — the sidecar *is* the
server. I ran the full installer non-interactively with VPN enabled and a throwaway password,
and got a working gateway.

```
[OK] VPN enabled (IKEv2 UDP 500/4500 + L2TP UDP 1701, egress pinned to working proxy)
  Generated IPsec PSK: PegDjm2z6XnODf0yUnmpcrM5   (auto-generated, disposable)

docker logs v2prodock-vpn:
  Generating CA + server cert for vpn.local (this happens once)...
  Certs installed into swanctl store.
  Wrote /config/vpn/apple.mobileconfig
  swanctl.conf rendered: domain=vpn.local ikev2-pool=10.10.10.0/24 l2tp-psk armed, 1 EAP user(s).
  ip_forward=1 already (compose sysctls).
  L2TP locked to IPsec (bare L2TP dropped).
  PPTP disabled (VPN_ENABLE_PPTP=0): TCP 1723 not served.
  iptables enforced: 10.10.10.0/24 -> tun0 only, un-NATed (direct egress DROPPED).
  iptables enforced: 10.10.11.0/24 -> tun0 only, un-NATed (direct egress DROPPED).
  charon up, ikev2-eap + l2tp-psk loaded.
  xl2tpd up on UDP 1701 (L2TP range 10.10.11.10-10.10.11.100).
  Pinning tun0 -> SOCKS v2prodock:27019 (NL | @Raydikalx | BCE758) ...
  Tunnel up: 10.10.10.0/24, 10.10.11.0/24 --table100--> tun0 -> v2prodock:27019
  VPN egress VERIFIED via NL | @Raydikalx | BCE758 (v2prodock:27019): 193.29.139.148

GET /vpn:
{"egress_ip":"193.29.139.148","enabled":true,"guarantee":"fail-closed: VPN subnet forwards to
tun0 only; verified means tun egress == upstream SOCKS egress","ikev2":"500/udp,4500/udp",
"l2tp":true,"plain_l2tp":false,"pptp":"off …","tun":"tun0","upstream_socks":"v2prodock:27019",
"verified":true}

config/vpn/: apple.mobileconfig  ca.crt  ca.key  server.crt  server.key  status.json
/v2prodock-vpn|running|unless-stopped|healthy
```

Egress `193.29.139.148` differs from the direct `187.13.208.194`, so the fail-closed pinning is
real, not asserted. Every path `install.sh:70–86` advertises (`show_status`) resolves to a real
artifact.

Also confirmed: `ensure_host_prereqs` wrote `/etc/modules-load.d/v2prodock.conf` with all 7
modules (inside the container's own rootfs — the real host was never touched), and reported
nothing about ufw/firewalld because neither binary exists there. The firewall branch is
therefore **unexercised**; see §6.

---

## 6. What I could NOT verify, and why

1. **`ensure_host_prereqs` on a real host.** Every step ran inside a container's own rootfs, so
   the mutations were contained. `modprobe`, `/etc/modules-load.d/v2prodock.conf`, `ufw allow`,
   `firewall-cmd --permanent` and the `nf_conntrack_pptp` `lsmod` check were never exercised
   against a real kernel/firewall. Per your instructions I did not run them on the Windows host
   or the Ubuntu WSL distro. The *code paths* were read and the VPN gating is correct by
   inspection (551, 579, 597, 621), but the firewall-rule text itself is unverified.
2. **`VPN_ENABLE_PPTP=1` and PPTP/GRE.** Needs host networking or a working
   `nf_conntrack_pptp`; the sidecar's own image build was also blocked by the SourceForge DNS
   issue. Not attempted.
3. **`docker-compose.host.yml` override.** Requires Linux + host networking. I confirmed the
   file parses as an override (compose requires ≥2.24 for `!reset`; host has 2.32.4) and that
   it is *not* auto-loaded, but I never brought the stack up under it.
4. **WSL2-specific branch.** The dind container's `/proc/version` contains `microsoft` and the
   WSL branch did fire in my `127.0.0.1` test, so `get_wsl_host_ip` → `fix_wsl_url` was
   exercised. But the real WSL2 path (`host.docker.internal` → Windows host IP) and the
   `hostname -I` LAN-IP hint at `install.sh:244` were not run against the actual Ubuntu distro.
5. **`git pull` / `stash` / `reset --hard` resync block (`install.sh:681–703`).** My `/work` had
   no `.git`, so the entire git-resync path was skipped. Its interaction with the untracked
   `config/` and `.env` (relied on to be preserved by `--include-untracked` + `.gitignore`) is
   **unverified**. Worth a dedicated test on a real divergence.
6. **`vpn-gateway/Dockerfile` as written.** I patched the pptpd fetch inside the sandbox copy.
   The repo file builds or not depending on whether the SourceForge mirror resolves at build
   time — which is itself a **fragility finding**: a single third-party redirect hostname
   standing between a user and a working install, with `docker compose build` failing the whole
   project because of it (F5).
7. **`free_port_squatter`'s eviction path.** Demonstrated unreachable (F5), but its kill
   logic itself was never exercised against a genuinely stale `docker-proxy` — deliberately, as
   that would mean killing a host process.
8. **Real-world scale and timing.** The installer's own 30–90s populate window
   (`install.sh:55`) was not measured under load; my container populated within that window
   from a healthy public subscription. Windows-host port publishing was never exercised (all
   ports were contained in dind), so the compose port map against a real Docker Desktop port
   proxy is unverified — as is any conflict with the host's existing `wslrelay.exe` listeners
   on `127.0.0.1:27000–27100`.

---

## 7. Bottom line

The installer's *design* is disciplined: it is well scoped (no `prune`, no volume removal, no
system-wide flags), it gates every firewall mutation behind `VPN_ENABLED`, it verifies
container state and restart policy instead of trusting `compose up`'s exit code, it refuses to
kill live or non-proxy port holders, and `PROJECT_CONTAINERS` has not drifted from compose.
The full install reached exit 0 in a disposable container and produced a stack that serves
real traffic through all four proxy paths with egress differing from direct; the VPN sidecar
came up healthy, generated certs, pinned tun0 to a working SOCKS and verified its own egress.

The recurring bug class is not the design — it is that **the success messages are not wired to
the success conditions**. `[OK] Started`, `[OK] Go installed`, `[OK] Built`,
`[OK] Zellij installed`, and `[OK] Updated subscriptions in .env` are all printed
unconditionally. F1, F2, F3, F6 and F7 are that single pattern, and each one produces a
green-looking transcript over a stack that does not work.