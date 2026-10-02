# OCPP charger load simulator

Simulates a fleet of OCPP 1.6J chargers (WebSocket) against a CSMS and plays a day of charging sessions
through it: Boot, Heartbeat, StatusNotification, Authorize, StartTransaction, MeterValues, StopTransaction.
Written in Go; one static binary, no runtime dependencies.

It only generates load. **It does not create any data in the target.** Chargers, customers, RFID tags, wallets
and vehicles must already exist there (see [What the target needs](#what-the-target-needs)).

> Only point it at a test environment. Never at production.

## Quick start on a Linux box (for example an EC2 instance)

Commands are for Amazon Linux 2023; adjust the package manager elsewhere.

```bash
# 1. Tools. go.mod needs a recent Go: install the version printed by `grep ^go go.mod`
#    from https://go.dev/dl/ (linux-amd64 tarball), then:
sudo dnf install -y git tmux
sudo tar -C /usr/local -xzf go<VERSION>.linux-amd64.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc && source ~/.bashrc

# 2. Get and build
git clone git@github.com:yeswanth-steam-a/bpp-scale-simulator.git
cd bpp-scale-simulator
make build            # bin/simulator, bin/probe, bin/mockcsms, bin/seedgen
make test             # optional: go vet + unit tests

# 3. OS limits: every charger holds one socket (file descriptor)
ulimit -n 200000                                          # this shell only
sudo sysctl -w net.ipv4.ip_local_port_range="1024 65535"  # more source ports
```

To make the file limit permanent, add to `/etc/security/limits.d/99-sim.conf` and log in again:
```
*  soft  nofile  200000
*  hard  nofile  200000
```

### Check the setup before any load

```bash
# a) No network needed: print what the run would do (sessions per hour, peak parallel sessions)
./bin/simulator -config configs/newenv-stage50.yaml -plan

# b) Local smoke test against the built-in fake CSMS (2,000 chargers, 24h squeezed into ~2 minutes)
make smoke

# c) One real charger, one real session, raw frames printed. Needs the charger and tag to exist in the target.
./bin/probe -target ws://<csms-host>:9888/csms/ -id LT-000001 -tag LTTAG000001 -connector 1 -hold 15s -v
```
If (c) prints `Accepted` for boot, authorize and start, the target is ready for load.
A `handshake HTTP 404` means that charger id is not registered in the target.

### Run a staged test

Run inside `tmux` so it survives a dropped SSH session. Start small and step up:

```bash
tmux new -s sim
ulimit -n 200000
./bin/simulator -config configs/newenv-stage.yaml       # 5 chargers, 5 parallel sessions
./bin/simulator -config configs/newenv-stage10.yaml     # 10
./bin/simulator -config configs/newenv-stage50.yaml     # 50
./bin/simulator -config configs/newenv-stage1000.yaml   # 1,000
```
Each stage connects N chargers, starts N sessions within a minute, each lasting about 3 minutes,
then waits for the stop replies, prints a summary and exits. Expect 5 to 15 minutes per stage.
Detach with `Ctrl-b d`, reattach with `tmux attach -t sim`; stop early with `Ctrl-c` (it shuts down cleanly).

Override settings without editing a file:
```bash
./bin/simulator -config configs/newenv-stage50.yaml -target ws://other-host:9888/csms/ -chargers 20 -duration 2m
```

### The full run (24 hours)

`configs/default.yaml`: 50,000 chargers, 10,000 sessions over 24 hours, about 2,000 in parallel at the peak,
with background disconnects and faults. Before starting it:

1. The target must hold all 50,000 chargers (`LT-000001` to `LT-050000`) and 4,000 customers
   (tags `LTTAG000001` to `LTTAG004000`), seeded with the same fleet size as `chargers:` in the config.
2. Check the plan: `./bin/simulator -plan` should show 10,000 sessions and a peak near 2,000.
3. One machine may not hold 50,000 sockets comfortably. Split across hosts with shards; every host uses the
   same config and its own index:
   ```bash
   ./bin/simulator -shard-index 0 -shard-count 4   # host 1
   ./bin/simulator -shard-index 1 -shard-count 4   # host 2, and so on
   ```
   Shards plan identically and each takes its own slice of chargers, tags and sessions. No coordination needed.
4. Measure first: run 5,000 chargers on the box you plan to use and watch memory and CPU before committing to 24 hours.

## Reading the results

**Progress line** (printed every 10 seconds):

| Field | Meaning |
|---|---|
| `connected` | chargers with an open WebSocket (`peak` in brackets) |
| `sessions` | sessions charging right now (`peak` in brackets) |
| `started` / `done` | sessions the server accepted / that finished charging |
| `aborted` | sessions that failed after starting (for example the connection dropped mid-start) |
| `skipped` | planned sessions with no free charger or tag when their time came |
| `auth_rej` | Authorize answered with anything but Accepted |
| `conn_fail`, `disc` | failed connection attempts, dropped connections |
| `callerr`, `timeouts` | CALLERROR replies, calls with no reply within `call_timeout` |
| `sent`, `recv`, `queued` | messages sent / received, messages buffered while offline |

**Summary file:** `results/<name>.json.shard<N>` (name from `summary_file`) has the totals and, per message type,
count, mean, p50, p95 and p99 latency. Latencies come from power-of-two buckets, so read them as upper bounds
(8389 ms means "between 4.2 and 8.4 seconds").

**Live metrics:** with `metrics_addr: ":2112"` the simulator serves Prometheus text at
`http://<host>:2112/metrics`. A Grafana Cloud setup is described in [`deploy/GRAFANA.md`](deploy/GRAFANA.md).

**Checking the other side.** A run is only half the story: also look at the target's own records, since the
CSMS can answer a message and still fail later. For example, compare `started` with the number of sessions
the backend actually created, and check that none are left open.

## What the target needs

| Thing | Requirement |
|---|---|
| Chargers | one row per charger id (`LT-000001`...) registered with the backend and the OCPP server, status Active. An unknown id is refused with HTTP 404 at the WebSocket handshake |
| Charger types | the simulator profiles (connector count, power) should match the seeded chargers; `configs/default.yaml` uses 45% AC 7 kW (1 connector), 30% AC 22 kW, 17% DC 60 kW, 8% DC 150 kW (2 connectors each). The staged configs use AC 7 kW only |
| Customers | one per tag (`LTTAG000001`...), tag type **`rfid`**, active, not expired, a wallet balance, a default vehicle |
| Tag type | `remote_id` tags are accepted by the OCPP server but create no backend session, so they test nothing |

The simulator never writes to the target's database or API. Seeding is done separately, by SQL, from a
machine that can reach the database (see `CLAUDE.md`, section "Seeding the target environment").

## Configuration reference

All settings are in one YAML file (see `configs/default.yaml` for a commented example).

| Setting | Meaning |
|---|---|
| `target` | WebSocket base URL ending in `/csms/`; the charger id is appended |
| `chargers` | fleet size (across all shards) |
| `id_prefix` | charger ids are `<prefix>` + 6 digits (`LT-000001`) |
| `ids_file` | optional: use these existing charger ids (one per line) instead of generating them |
| `duration` | length of the session plan (session starts are spread over this) |
| `time_scale` | above 1 compresses time (`144` plays 24h in 10 minutes). For smoke tests only |
| `seed` | makes the session plan reproducible |
| `connect_rate` | new connections per second during ramp-up, per shard (`50` connects 1,000 chargers in 20 s) |
| `call_timeout` | a call with no reply after this long counts as a timeout |
| `sessions.total`, `sessions.curve` | sessions over the run, and relative start rate per slot (24 values = hours of the day) |
| `idtags.prefix`, `idtags.pool` | tags are `<prefix>` + 6 digits; `pool` is how many exist. A tag serves one session at a time |
| `idtags.list` / `idtags.file` | optional: use exactly these existing tags (`VID:` entries skipped) |
| `noise.*` | background disconnects and connector faults per charger per day |
| `profiles[]` | charger types: `share` of the fleet, `connectors`, `max_power_w`, `voltage`, `meter_interval`, session length (`session_mean`, `_sigma`, `_min`, `_max`) and `session_weight` |

Peak parallel sessions comes from start rate times session length, not from one setting. After changing the curve,
profiles or session lengths, run `./bin/simulator -plan` and read the "peak parallel sessions" line.

## What a simulated session does

Plug in (Preparing), Authorize with the tag, StartTransaction, Charging, a MeterValues reading every
`meter_interval` (energy, power, current, voltage; SoC on DC), then Finishing, StopTransaction, Available.
Energy follows power times time, in whole watt-hours (the CSMS flags sessions whose readings exceed the stop
meter, even by 1 Wh). If the connection drops, a charger reconnects with backoff, boots again and sends the
readings and stop it buffered while offline.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `handshake HTTP 404` / many `conn_fail` | charger ids are not seeded or not Active in the target |
| `auth_rej` above 0 | tag missing, expired, not `rfid` type, or user has no wallet or vehicle |
| `too many open files` | raise `ulimit -n` in the shell that starts the simulator |
| `cannot assign requested address` | out of source ports: widen `ip_local_port_range`, or shard across hosts |
| `timeouts` growing, replies slow | the target is saturated. Note the load level and look at the target's CPU, DB and queues |
| Sessions stay `STARTED` in the CMS | stop replies did not arrive before the run ended, or timed out on a saturated target |
| CMS shows sessions as "Interrupted" while running | the backend never recorded meter values for them (the meter-value message path to the backend is not running in that environment) |
| `StopTransaction` takes 10 to 17 s even at low load | seen on the current target at every load level; it is slow server-side |

## Other tools

| Command | Purpose |
|---|---|
| `bin/probe` | connect one or more chargers, run a session, print raw frames (`-id A,B,C -tag T -v`); `-stop-tx N -meter-stop W -tag T` closes a transaction left open |
| `bin/mockcsms` | tiny fake CSMS for offline tests (`-addr :19898`) |
| `bin/seedgen` | legacy: writes SQL for the OCPP server's own database schema; not used for the current target |

Secrets (`.env.seed`) and generated output (`bin/`, `results/`) are git-ignored. Never commit credentials.
