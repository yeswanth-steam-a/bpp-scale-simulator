#!/usr/bin/env bash
# Connection-limit probe: opens N OCPP charger connections (no charging sessions), holds them briefly, and reports how
# many succeeded, when failures started, why they failed, and whether this machine's network limits were hit.
#
# Usage (on the load-generator EC2, from the repo root, after `make build`):
#   scripts/conn-probe.sh [CHARGERS=50000] [RATE=200] [HOLD=2m]
#   e.g. scripts/conn-probe.sh 50000 200 2m
#   TARGET=ws://host:port/csms/ scripts/conn-probe.sh ...   (optional: override the target)
#
# Uses configs/connect-50000.yaml as the base (same target, chargers LT-000001.., real type mix) with the
# charger count, connect rate and hold time replaced. Output: results/conn-probe-<time>.log
set -uo pipefail

CHARGERS=${1:-50000}
RATE=${2:-200}
HOLD=${3:-2m}
STAMP=$(date +%H%M%S)
CFG=$(mktemp /tmp/conn-probe-XXXX.yaml)
LOG=results/conn-probe-$STAMP.log
mkdir -p results

sed -e "s/^chargers: .*/chargers: $CHARGERS/" \
    -e "s/^connect_rate: .*/connect_rate: $RATE/" \
    -e "s/^warmup: .*/warmup: $HOLD/" \
    -e "s#^summary_file: .*#summary_file: \"results/conn-probe-$STAMP.json\"#" \
    configs/connect-50000.yaml > "$CFG"
[ -n "${TARGET:-}" ] && sed -i.bak "s#^target: .*#target: $TARGET#" "$CFG"

NIC=$(ip -o link 2>/dev/null | awk -F': ' '$2!="lo"{print $2; exit}')
counters() { ethtool -S "$NIC" 2>/dev/null | grep -iE "allowance_exceeded" | tr -s ' ' || echo "  (ethtool not available)"; }

echo "== pre-checks ($(hostname), $(date +%T))"
echo "  chargers=$CHARGERS rate=$RATE/s hold=$HOLD target=$(grep '^target:' "$CFG" | awk '{print $2}')"
echo "  source port range: $(sysctl -n net.ipv4.ip_local_port_range | tr -s '\t ' ' ')"
ulimit -n 65535 2>/dev/null || true
echo "  open-file limit (ulimit -n): $(ulimit -n)"
[ "$(ulimit -n)" -lt $((CHARGERS + 1000)) ] && echo "  WARNING: open-file limit is below chargers + 1000"
lo=$(sysctl -n net.ipv4.ip_local_port_range | awk '{print $1}'); hi=$(sysctl -n net.ipv4.ip_local_port_range | awk '{print $2}')
[ $((hi - lo)) -lt "$CHARGERS" ] && echo "  WARNING: port range too small; run: sudo sysctl -w net.ipv4.ip_local_port_range=\"1024 65535\""
echo "  memory: $(free -m 2>/dev/null | awk '/Mem:/{print $7" MB available of "$2" MB"}')"
echo "  EC2 network-limit counters before ($NIC):"; counters | sed 's/^/   /'
BEFORE=$(counters)

echo "== running (progress every 10 s: time, connected, failed connects)"
./bin/simulator -config "$CFG" 2>&1 | tee "$LOG" | awk '
  /connect error/ { print "  " $0; next }
  /t=/ { for (i=1;i<=NF;i++) { if ($i ~ /^t=/) t=$i; if ($i ~ /^connected=/) c=$i; if ($i ~ /^conn_fail=/) f=$i }
         print "  " $2 "  " t "  " c "  " f; fflush() }
  /warm-up|ramp complete|WARNING|run finished/ { print "  " $0 }'

echo "== result"
first_fail=$(grep -m1 -E 'conn_fail=[1-9]' "$LOG" | grep -oE '^[0-9/]+ [0-9:]+ t=[0-9ms]+ connected=[0-9]+' || true)
echo "  peak connected: $(grep 'run finished' "$LOG" | grep -o 'connected=0 (peak [0-9]*)' | grep -o '[0-9]*)' | tr -d ')') of $CHARGERS"
echo "  total failed connects: $(grep 'run finished' "$LOG" | grep -o 'conn_fail=[0-9]*' | cut -d= -f2)"
echo "  first failure at: ${first_fail:-none}"
echo "  connect error kinds:"; grep 'connect error' "$LOG" | sed -E 's/^[0-9/]+ [0-9:]+ //' | sort | uniq | head -10 | sed 's/^/    /' || true
echo "  EC2 network-limit counters after ($NIC):"; counters | sed 's/^/   /'
[ "$BEFORE" != "$(counters)" ] && echo "  >> counters CHANGED during the run: this machine hit an AWS network allowance"
echo "  memory now: $(free -m 2>/dev/null | awk '/Mem:/{print $7" MB available"}')"
echo "  full log: $LOG"
rm -f "$CFG" "$CFG.bak"
