# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Purpose

Load simulator for the EV CSMS (sibling repos `../saevocpp` OCPP 1.6J server, `../ev-backend`, `../network-bff-aggregation`). It backs a BP Pulse RFP claim about fleet size and parallel sessions. Agreed scope: **chargers only** (no app/REST traffic), OCPP 1.6J over plain `ws://`, charger-initiated sessions (Authorize -> StartTransaction), target ~40k connected chargers, 24h run, ~10k sessions, ~2k peak parallel. Reporting/dashboards were deliberately deferred; the target environment is being built by someone else.

## Seeding the target environment (new env, DB `suite_scale_simulator`)

- DB is reachable only through an SSM tunnel (`aws ssm start-session ... AWS-StartPortForwardingSessionToRemoteHost`, local port 5433). Credentials live in the gitignored `.env.seed` (`SEED_DB_URL`, `SEED_API_BASE`, `SEED_ACCESS_TOKEN`, ...); never commit it or print its values.
- The API can create stations, chargers, connectors, tariffs, level items and charger categories, but NOT customers, RFID tags, wallets or vehicles, so those go in by SQL. `seed/stations.sql`, `seed/fleet-chargers.sql` and `seed/customers.sql` clone rows that were created through the API (charger templates `LT-000001`, `LT-TPLAC22`, `LT-TPLDC60`, `LT-TPLDC150`; customer 1). Run them with `psql -v first= -v last= ...` (usage is in each file's header). `fleet-chargers.sql -v total=` must equal the simulator's `chargers:`.
- **Test-only indexes live in this DB on purpose** (user's decision, 2026-10-03): `seed/test-indexes.sql` lists them. Without them every OCPP message did full-table scans and the stack stalled at ~500 chargers. Re-apply that file on any new copy of the DB. Production may lack them: an open question for the BP report.
- The original 1,000 chargers were moved out of "LoadTest Station 1" into 4-charger stations (`seed/move-first-1000.sql`); one station with 1,000 chargers made the backend's per-station lookup pathological.
- `seed/teardown-loadtest.sql` removes seeded chargers, customers and sessions (not the API-created station, tariff, levels or business units).
- Only `rfid`-type tags create real backend sessions; `remote_id` tags get Accepted by the OCPP server with no backend session. Whole-Wh meter values only: a reading above the stop meter, even by 1 Wh, makes the backend auto-close the session as suspicious.
- Staged ramp agreed with the user: 5, 10, 50, 100, 500 sessions, confirming with the user after each stage; the full 50k seed only after 500 passes. Stages 5 and 10 passed.
- Deploy target: EC2 `i-03e4d340bd383dfe7` ("simulator", t3.medium, Amazon Linux 2023, SSH as `ec2-user`; key pair `iris_network_dev_stage`, no SSM role).

## Commands

- `make build` builds `bin/{simulator,seedgen,mockcsms}`; `make test` runs vet + tests
- `go run ./cmd/simulator -plan` prints planned sessions, hourly starts and peak parallelism without connecting (use this after editing `sessions.curve` or profiles)
- `make smoke` runs 2000 chargers with 24h compressed to ~2 min against `cmd/mockcsms`
- `go run ./cmd/seedgen -out seed -tenant <t>` writes `seed/seed.sql` and `seed/teardown.sql`
- Real run: `./bin/simulator -config configs/default.yaml -target ws://HOST:9898/csms/`; shard across hosts with `-shard-index i -shard-count n`
- Raise `ulimit -n` (>= 100000) on the load generator before a 40k run
- `scripts/guarded-run.sh CONFIG` runs the simulator and stops it when timeouts/Authorize rejections spike (protects the account-wide Lambda limit shared with production); `GUARD_CONSEC`/`GUARD_CONN_CONSEC` make it tolerant for long soaks
- `scripts/log-truncate.sh [THRESHOLD_MB] [INTERVAL_S]` keeps the OCPP server's `saev_ocpp_log` (two rows per charger message, ~1.6 GB per 10 min at 30k parallel) from filling the shared RDS: saves response-time medians, then TRUNCATEs above the threshold. Run it inside AWS (simulator EC2, `LOG_DB_URL` pointing at the RDS proxy) for anything longer than a supervised test

## Architecture

- `internal/charger`: one goroutine per charger owns all its state (no locks); a reader goroutine feeds frames in, timers post closures to `c.events`. Handles boot, heartbeat (interval from BootNotification), status, session lifecycle, MeterValues (battery/SoC model, DC taper), offline queue (MeterValues/StopTransaction buffered while disconnected, flushed after next accepted Boot) and reconnect with jittered backoff.
- `internal/fleet`: `BuildPlan` draws all session starts from the hourly demand curve using a fixed seed. Every shard builds the identical plan and keeps `i % shardCount`, so shards need no coordination. The plan clock starts only after ~99% of chargers have connected. `noise()` injects background disconnects and connector faults (no storms by design).
- `internal/config`: single YAML shared by simulator and seedgen. `ProfileFor(i)` and `ChargePointID(i)` must stay deterministic because seedgen and the simulator both rely on them. Pool tags above `ValidTagCount()` are intentionally not seeded, so the CSMS answers Invalid.
- `internal/metrics`: atomic counters and log2 latency histograms, a 10 s log line, native Prometheus histograms at `/metrics` (scraped by Alloy into Grafana Cloud, see `deploy/GRAFANA.md`), JSON summary per shard at exit.
- `time_scale` divides all real timers (session length, meter and heartbeat intervals); OCPP energy math still uses simulated time.

## CSMS facts the simulator depends on (from `../saevocpp`)

Verified against `origin/iris_suite_dev` (9 months old at last fetch; the checked-out `ocpp-cms-integration` branch is ~3.5 years old and misses the backend calls below). Wire formats of Boot/Status/Start/Stop/MeterValues beans are identical on both branches, but behaviour is not: on `iris_suite_dev` the OCPP server calls `ev-backend` over HTTP (`api.base-url`) on Authorize (`/ocpp/validateAuthorize`), StartTransaction (`/ocpp/startOcppSession`, synchronous; no sessionId means the transaction is stopped and Invalid returned), StopTransaction (`/ocpp/stopOcppSession`), plus async calls for connector status, heartbeat, metadata and boot config. MeterValues are also published to RabbitMQ. Every connect/disconnect and message writes `FormattedMessageLog` rows. So idTags and charge points must exist in the backend as well as in the `saev_ocpp_*` tables, or sessions are rejected.


- WebSocket path `/csms/<chargePointId>`, subprotocol `ocpp1.6`. An unknown or non-`Active` charge point id gets HTTP 404 at handshake. No handshake auth exists in the code read.
- Every message other than BootNotification needs a prior accepted Boot or the server returns CALLERROR NotSupported.
- StartTransaction is refused if the connector already has a running transaction, or the idTag is not `Accepted`.
- Seed tables: `public.saev_ocpp_charge_point` (status must be `Active`) and `public.saev_ocpp_id_tag` per `ev-backend/DB/scripts/x_main/001_v_1.0.0/create/001_xmain_schema_v1.0.0.sql`. `saevocpp/src/main/resources/create-db-structure.sql` uses a different `ocpp.*` schema (dev-only).
- **Charger types are seeded in contiguous blocks over 50,000 ids**: LT-000001..022500 AC 7.4 kW, 022501..037500 AC 22 kW, 037501..046000 DC 60 kW (2), 046001..050000 DC 150 kW; since 2026-10-04 every charger has 2 connectors (100,000 in total: `seed/add-second-connector.sql` gave the AC 7.4 kW block its second one, and template LT-000001 now has 2, so new clones of it get 2). `ProfileFor(i)` spreads profiles by position over `chargers:`, so a multi-profile config with fewer than 50,000 chargers must use an `ids_file` listing ids from each block in profile order (see `configs/mix-3000-ids.txt`). Otherwise the simulator uses the wrong power (e.g. DC power on 7.4 kW chargers) and the backend refuses or flags the sessions. Single-profile AC 7 kW configs are fine on LT-000001..022500 (those with `connectors: 1` just never use connector 2).
