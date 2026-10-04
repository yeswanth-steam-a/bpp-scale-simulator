#!/usr/bin/env bash
# Runs the simulator and stops it (SIGINT) as soon as the backend degrades, so an overloaded stack cannot fill the
# AWS account's shared Lambda concurrency (which also serves production). Checks every 10 s line of the simulator log:
#   - call timeouts or rejected Authorizes grew by more than the limits since the previous line, for GUARD_CONSEC
#     lines in a row (default 1: trip on the first bad 10 s);
#   - connected chargers below 98% of the run's peak for GUARD_CONN_CONSEC lines in a row (default 1).
# For long soaks, require persistence so one spike or a planned OCPP restart (chargers reconnect) does not end the run:
#   GUARD_CONSEC=6 GUARD_CONN_CONSEC=60 scripts/guarded-run.sh CONFIG 50 100   (1 min of errors / 10 min disconnected)
#
# Usage (repo root, after `make build`): scripts/guarded-run.sh CONFIG [MAX_TIMEOUTS=10] [MAX_AUTH_REJ=20]
# TARGET=ws://HOST:PORT/csms/ overrides the config's target (e.g. when DNS is stale).
# Log: results/<config name>-<HHMM>.log
set -uo pipefail
CFG=${1:?usage: scripts/guarded-run.sh CONFIG [MAX_TIMEOUTS] [MAX_AUTH_REJ]}
MAXT=${2:-10}; MAXA=${3:-20}
NERR=${GUARD_CONSEC:-1}; NCONN=${GUARD_CONN_CONSEC:-1}
LOG=results/$(basename "$CFG" .yaml)-$(date +%H%M).log
mkdir -p results

./bin/simulator -config "$CFG" ${TARGET:+-target "$TARGET"} > "$LOG" 2>&1 &
PID=$!
echo "simulator pid $PID, log $LOG (guard: >$MAXT timeouts or >$MAXA auth rejections per 10 s for $NERR line(s);" \
     "connected <98% of peak for $NCONN line(s))"
trap 'kill -INT $PID 2>/dev/null' INT TERM

field() { grep -o " $1=[0-9]*" <<<"$2" | head -1 | cut -d= -f2; }
pt=0; pa=0; peak=0; berr=0; bconn=0; tripped=0
while kill -0 $PID 2>/dev/null; do
  sleep 10
  line=$(grep ' t=' "$LOG" | tail -1)
  [ -z "$line" ] && continue
  echo "${line:11:200}"
  t=$(field timeouts "$line"); a=$(field auth_rej "$line"); c=$(field connected "$line")
  [ "$c" -gt "$peak" ] && peak=$c
  err=""
  [ $((t - pt)) -gt "$MAXT" ] && err="call timeouts +$((t - pt)) in 10 s"
  [ $((a - pa)) -gt "$MAXA" ] && err="${err:+$err, }Authorize rejected +$((a - pa)) in 10 s"
  if [ -n "$err" ]; then berr=$((berr + 1)); else berr=0; fi
  if [ "$peak" -gt 1000 ] && [ "$c" -lt $((peak * 98 / 100)) ]; then bconn=$((bconn + 1)); else bconn=0; fi
  why=""
  [ "$berr" -ge "$NERR" ] && why="$err (bad for $berr line(s))"
  [ "$bconn" -ge "$NCONN" ] && why="${why:+$why, }connected $c < 98% of peak $peak for $bconn line(s)"
  if [ -n "$why" ] && [ $tripped -eq 0 ]; then
    echo "GUARD STOPPED THE RUN at $(date +%T): $why" | tee -a "$LOG"
    kill -INT $PID; tripped=1
  elif [ -n "$err" ] || [ "$bconn" -gt 0 ]; then
    echo "guard warning $(date +%T): ${err:-connected $c of peak $peak}" | tee -a "$LOG"
  fi
  pt=$t; pa=$a
done
grep -E 'run finished|GUARD' "$LOG" | tail -2
