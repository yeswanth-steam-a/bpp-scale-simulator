#!/usr/bin/env bash
# Runs the simulator and stops it (SIGINT) as soon as the backend degrades, so an overloaded stack cannot fill the
# AWS account's shared Lambda concurrency (which also serves production). Trips when, since the previous 10 s line,
# call timeouts or rejected Authorizes grow by more than the limits, or connected chargers drop by more than 2%.
#
# Usage (repo root, after `make build`): scripts/guarded-run.sh CONFIG [MAX_TIMEOUTS=10] [MAX_AUTH_REJ=20]
# Log: results/<config name>-<HHMM>.log
set -uo pipefail
CFG=${1:?usage: scripts/guarded-run.sh CONFIG [MAX_TIMEOUTS] [MAX_AUTH_REJ]}
MAXT=${2:-10}; MAXA=${3:-20}
LOG=results/$(basename "$CFG" .yaml)-$(date +%H%M).log
mkdir -p results

./bin/simulator -config "$CFG" > "$LOG" 2>&1 &
PID=$!
echo "simulator pid $PID, log $LOG (guard: >$MAXT timeouts or >$MAXA auth rejections per 10 s)"
trap 'kill -INT $PID 2>/dev/null' INT TERM

field() { grep -o " $1=[0-9]*" <<<"$2" | head -1 | cut -d= -f2; }
pt=0; pa=0; pc=0; tripped=0
while kill -0 $PID 2>/dev/null; do
  sleep 10
  line=$(grep ' t=' "$LOG" | tail -1)
  [ -z "$line" ] && continue
  echo "${line:11:200}"
  t=$(field timeouts "$line"); a=$(field auth_rej "$line"); c=$(field connected "$line")
  why=""
  [ $((t - pt)) -gt "$MAXT" ] && why="call timeouts +$((t - pt)) in 10 s"
  [ $((a - pa)) -gt "$MAXA" ] && why="${why:+$why, }Authorize rejected +$((a - pa)) in 10 s"
  [ "$pc" -gt 1000 ] && [ "$c" -lt $((pc * 98 / 100)) ] && why="${why:+$why, }connected dropped $pc -> $c"
  if [ -n "$why" ] && [ $tripped -eq 0 ]; then
    echo "GUARD STOPPED THE RUN at $(date +%T): $why" | tee -a "$LOG"
    kill -INT $PID; tripped=1
  fi
  pt=$t; pa=$a; pc=$c
done
grep -E 'run finished|GUARD' "$LOG" | tail -2
