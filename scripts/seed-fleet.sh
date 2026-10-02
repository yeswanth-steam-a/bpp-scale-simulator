#!/usr/bin/env bash
# Seeds the full fleet in chunks: stations, chargers (type mix from the templates), customers.
# Usage: scripts/seed-fleet.sh            (needs .env.seed with SEED_DB_URL and SEED_TENANT_ID, DB tunnel open)
# Defaults match configs/default.yaml: 50,000 chargers, 4 per station, 4,000 customers.
# Chargers 1..1000 and customers 1..1000 already exist (all in station 1), so this starts at 1001.
# Safe to re-run: a chunk whose last charger (or customer) already exists is skipped, and a chunk whose
# connection is killed (this shared RDS has scheduled jobs that terminate connections) is retried; each
# chunk is one transaction, so a failed chunk leaves nothing behind. SKIP_STATIONS=1 skips the station step.
set -uo pipefail
cd "$(dirname "$0")/.."
set -a; . ./.env.seed; set +a
export PATH=/opt/homebrew/opt/postgresql@15/bin:$PATH
TOTAL=${TOTAL:-50000}; PER_STATION=${PER_STATION:-4}; FIRST=${FIRST:-1001}; CHUNK=${CHUNK:-5000}
CUST_FIRST=${CUST_FIRST:-1001}; CUST_LAST=${CUST_LAST:-4000}
PSQL=(psql "$SEED_DB_URL" -v ON_ERROR_STOP=1 -q)
ts() { date +%T; }
exists() { [ "$(psql "$SEED_DB_URL" -At -c "$1" 2>/dev/null)" = "1" ]; }
run_chunk() {  # $1 = description, rest = psql args; retries up to 3 times
  local what=$1; shift
  for attempt in 1 2 3; do
    if "${PSQL[@]}" "$@"; then return 0; fi
    echo "$(ts) $what failed (attempt $attempt), retrying in 15s"; sleep 15
  done
  echo "$(ts) $what FAILED after 3 attempts, stopping"; exit 1
}

ST_FIRST=$(( (FIRST - 1) / PER_STATION + 1 )); ST_LAST=$(( (TOTAL - 1) / PER_STATION + 1 ))
if [ "${SKIP_STATIONS:-0}" != "1" ]; then
  echo "$(ts) stations $ST_FIRST..$ST_LAST"
  run_chunk "stations" -v first=$ST_FIRST -v last=$ST_LAST -f seed/stations.sql
  echo "$(ts) stations done"
fi

n=$FIRST
while [ $n -le $TOTAL ]; do
  last=$(( n + CHUNK - 1 )); [ $last -gt $TOTAL ] && last=$TOTAL
  code=$(printf 'LT-%06d' $last)
  if exists "select 1 from asset_db.saev_charge_point where ocpp_charge_point_id='$code'"; then
    echo "$(ts) chargers $n..$last already present, skipped"; n=$(( last + 1 )); continue
  fi
  t0=$(date +%s)
  run_chunk "chargers $n..$last" -v first=$n -v last=$last -v total=$TOTAL -v prefix=LT- -v perstation=$PER_STATION -f seed/fleet-chargers.sql
  echo "$(ts) chargers $n..$last done in $(( $(date +%s) - t0 ))s; db connections now $(psql "$SEED_DB_URL" -At -c 'select count(*) from pg_stat_activity')/402"
  n=$(( last + 1 )); sleep 5
done

ctag=$(printf 'LTTAG%06d' $CUST_LAST)
if exists "select 1 from public.saev_rfid where rfidnumber='$ctag'"; then
  echo "$(ts) customers $CUST_FIRST..$CUST_LAST already present, skipped"
else
  echo "$(ts) customers $CUST_FIRST..$CUST_LAST"
  run_chunk "customers" -v first=$CUST_FIRST -v last=$CUST_LAST -v tenant="$SEED_TENANT_ID" -f seed/customers.sql
fi
echo "$(ts) SEED COMPLETE"
