#!/usr/bin/env bash
# Keeps the OCPP server's message log (public.saev_ocpp_log, two rows per charger message) from filling the shared RDS
# during long load tests. Every INTERVAL seconds it reads the table's size (instant); above THRESHOLD_MB it first saves
# median / p95 / max response time per OCPP action from the newest ~300k rows (for the report), then TRUNCATEs the table
# (frees disk at once with almost no I/O, unlike DELETE). Only ever touches that one table, and refuses to run against
# any database other than suite_scale_simulator.
#
# Usage (repo root, e.g. on the simulator EC2 in its own tmux window, started before the simulator):
#   LOG_DB_URL='postgres://user:pass@iris-internal.proxy-crjdtlhu0jv9.ap-south-1.rds.amazonaws.com:5432/suite_scale_simulator' \
#     scripts/log-truncate.sh [THRESHOLD_MB=1500] [INTERVAL_S=60]
# Without LOG_DB_URL it uses SEED_DB_URL from .env.seed (on a laptop that URL goes through the SSM tunnel).
# NO_MEDIANS=1 skips the response-time snapshot. Stop with Ctrl-C.
# Logs: results/log-truncate.log (one line per check), results/log-truncate-medians.log (one block per truncation).
set -uo pipefail
THRESHOLD_MB=${1:-1500}; INTERVAL=${2:-60}
LOG=results/log-truncate.log; MED=results/log-truncate-medians.log
mkdir -p results

if [ -z "${LOG_DB_URL:-}" ] && [ -f .env.seed ]; then set -a; . ./.env.seed; set +a; LOG_DB_URL=${SEED_DB_URL:-}; fi
[ -n "${LOG_DB_URL:-}" ] || { echo "set LOG_DB_URL (or SEED_DB_URL in .env.seed)"; exit 1; }
command -v psql >/dev/null || { echo "psql not found (Amazon Linux: sudo dnf install -y postgresql15)"; exit 1; }

q() { psql "$LOG_DB_URL" -At -v ON_ERROR_STOP=1 -c "set statement_timeout=${2:-15000}" -c "$1" 2>&1 | grep -v '^SET$'; }
db=$(q "select current_database()") || { echo "cannot connect: $db"; exit 1; }
[ "$db" = "suite_scale_simulator" ] || { echo "refusing to run: connected to database '$db', expected suite_scale_simulator"; exit 1; }

MEDIANS="with r as (select id, charge_point_id cp, call_action a, created_at, message::jsonb->>1 mid from public.saev_ocpp_log
  where id > (select max(id)-300000 from public.saev_ocpp_log)
    and call_action ~ '^(Heartbeat|MeterValues|StatusNotification|Authorize|StartTransaction|StopTransaction)(Request|Response)\$'),
w as (select a, created_at, lead(a) over n na, lead(created_at) over n nt from r window n as (partition by cp, mid order by created_at, id)),
p as (select replace(a,'Request','') act, extract(epoch from nt-created_at) d from w where a like '%Request' and na=replace(a,'Request','Response'))
select act, count(*), round(percentile_cont(0.5) within group (order by d)::numeric,2),
  round(percentile_cont(0.95) within group (order by d)::numeric,2), round(max(d)::numeric,2) from p group by act order by act"

echo "$(date '+%F %T') start: threshold ${THRESHOLD_MB} MB, every ${INTERVAL}s, db $db" | tee -a "$LOG"
trap 'echo "$(date "+%F %T") stopped" | tee -a "$LOG"; exit 0' INT TERM
while :; do
  bytes=$(q "select pg_total_relation_size('public.saev_ocpp_log')")
  mb=0; [[ "$bytes" =~ ^[0-9]+$ ]] && mb=$(( bytes / 1048576 ))
  if ! [[ "$bytes" =~ ^[0-9]+$ ]]; then
    echo "$(date '+%F %T') size check failed: ${bytes:0:200}" | tee -a "$LOG"
  elif [ "$bytes" -gt $(( THRESHOLD_MB * 1048576 )) ] && [ "$(q "select exists(select 1 from public.saev_ocpp_log)")" = t ]; then
    if [ -z "${NO_MEDIANS:-}" ]; then
      { echo "== $(date '+%F %T') before truncate at ${mb} MB  (action | n | median s | p95 s | max s)"; q "$MEDIANS" 120000; } >> "$MED"
    fi
    ok=FAILED
    for i in 1 2 3; do
      if psql "$LOG_DB_URL" -q -v ON_ERROR_STOP=1 -c "set lock_timeout='3s'" -c "truncate public.saev_ocpp_log" >/dev/null 2>&1; then ok=ok; break; fi
      sleep 5
    done
    echo "$(date '+%F %T') ${mb} MB > ${THRESHOLD_MB} MB: truncate $ok" | tee -a "$LOG"
  else
    echo "$(date '+%F %T') ${mb} MB" >> "$LOG"
  fi
  sleep "$INTERVAL"
done
