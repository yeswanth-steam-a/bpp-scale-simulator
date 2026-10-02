#!/usr/bin/env bash
# Stepped ramp: runs the simulator at increasing sizes, waiting for the backend to drain between steps.
# Usage: scripts/ramp.sh "100 250 500 750 1000"      (needs .env.seed with SEED_DB_URL and the DB tunnel open)
# Each step: N chargers, N sessions starting at ~8/s, each lasting ~3 min. Stops the ramp if a step has
# more than 2% timeouts or the backend does not drain within 10 minutes.
set -u
cd "$(dirname "$0")/.."
set -a; . ./.env.seed; set +a
export PATH=/opt/homebrew/opt/postgresql@15/bin:$PATH
ulimit -n 65536
STEPS=${1:-"100 250 500 750 1000"}
TEMPLATE=configs/newenv-stage50.yaml
open_sessions() { psql "$SEED_DB_URL" -At -c "select count(*) from public.saev_charging_session where status='STARTED'" 2>/dev/null; }

for N in $STEPS; do
  DUR=$(( N / 8 )); [ "$DUR" -lt 30 ] && DUR=30
  CFG=results/ramp-$N.yaml
  sed -E -e "s/^chargers:.*/chargers: $N/" -e "s/^duration:.*/duration: ${DUR}s/" -e "s/^seed:.*/seed: $N/" \
         -e "s/^connect_rate:.*/connect_rate: 50/" -e "s#^summary_file:.*#summary_file: \"results/ramp-$N.json\"#" \
         -e "s/^  total:.*/  total: $N/" -e "s/^  pool:.*/  pool: $N/" "$TEMPLATE" > "$CFG"
  echo "=== $(date +%T) STEP $N: $N chargers, $N sessions over ${DUR}s"
  ./bin/simulator -config "$CFG" > results/ramp-$N.log 2>&1
  tail -1 results/ramp-$N.log | cut -c1-300
  python3 - "$N" <<'PY'
import json,sys
n=sys.argv[1]; d=json.load(open(f'results/ramp-{n}.json.shard0'))
print(f"  started={d['sessions_started']} done={d['sessions_done']} skipped={d['sessions_skipped']} auth_rej={d['auth_rejected']} timeouts={d['call_timeouts']} peak_sessions={d['peak_sessions']}")
for a in ['Authorize','StartTransaction','StopTransaction','Heartbeat']:
    v=d['latency'].get(a)
    if v: print(f"  {a:17} p50={v['p50_ms']:.0f}ms p95={v['p95_ms']:.0f}ms n={v['count']}")
sent=max(d['messages_sent'],1)
sys.exit(1 if d['call_timeouts']/sent>0.02 else 0)
PY
  RC=$?
  # wait for the backend to finish stopping sessions
  for i in $(seq 1 60); do O=$(open_sessions); [ "${O:-1}" = "0" ] && break; echo "  waiting for backend to drain: $O sessions still STARTED"; sleep 10; done
  O=$(open_sessions); echo "  open sessions after drain wait: ${O:-?}"
  if [ "$RC" != "0" ]; then echo "STOPPING RAMP: step $N had more than 2% timeouts"; exit 1; fi
  if [ "${O:-1}" != "0" ]; then echo "STOPPING RAMP: backend did not drain after step $N"; exit 1; fi
done
echo "=== RAMP COMPLETE $(date +%T)"
