#!/usr/bin/env bash
# Runs the bad-network scenarios and writes netem/results/results.md.
# Usage: netem/run.sh [scenario ...]   (default: all)
set -euo pipefail
cd "$(dirname "$0")"
DC="docker compose -f compose.yaml"
DUR=${DUR:-30s}
# MODE=synthetic (default): worst-case synthetic load at the full layer
# bitrates. MODE=trace: replay real VideoToolbox output recorded with
# examples/vtrace (netem/traces/*.json): real frame sizes, timing and
# keyframes from a real camera and a real desktop.
MODE=${MODE:-synthetic}
EXTRA=()
if [ "$MODE" = trace ]; then
  export CAM_SOURCE="-trace /traces/camera.json" SCREEN_SOURCE="-trace /traces/screen.json"
  EXTRA=(-content-gaps)
fi
WARMUP=${WARMUP:-10s}
mkdir -p results

$DC up -d --build --force-recreate >/dev/null
sleep 3

srv_if=$($DC exec -T server sh -c "ip -o -4 addr | awk '/10\.77\.2\./{print \$2}'")
pub_if=$($DC exec -T pub-cam sh -c "ip -o -4 addr | awk '/10\.77\.1\./{print \$2}'")

shape()    { $DC exec -T server tc qdisc replace dev "$srv_if" root "$@"; }
unshape()  { $DC exec -T server tc qdisc del dev "$srv_if" root 2>/dev/null || true
             $DC exec -T pub-cam tc qdisc del dev "$pub_if" root 2>/dev/null || true; }
uplink()   { $DC exec -T pub-cam tc qdisc replace dev "$pub_if" root "$@"; }

bench() { # name [extra netbench flags]
  local name=$1; shift
  $DC exec -T viewer netbench -addr 10.77.2.10:7710 -insecure -token "viewer-$name" -room call \
    -duration "$DUR" -warmup "$WARMUP" -scenario "$name" -json "/results/$MODE-$name.json" ${EXTRA[@]+"${EXTRA[@]}"} "$@"
}

run() {
  local name=$1
  unshape
  case $name in
    baseline) ;;
    loss2)    shape netem loss 2% ;;
    loss5)    shape netem loss 5% ;;
    delay100) shape netem delay 100ms 20ms distribution normal ;;
    cap3)     shape tbf rate 3mbit burst 16kb latency 100ms ;;
    cap1)     shape tbf rate 1mbit burst 8kb latency 100ms ;;
    capdrop)  shape tbf rate 6mbit burst 32kb latency 100ms
              DUR=45s bench "$name" -event-at 15s &
              sleep 15
              shape tbf rate 1mbit burst 8kb latency 100ms
              wait
              return ;;
    uplink1)  uplink tbf rate 1mbit burst 8kb latency 100ms ;;
    *) echo "unknown scenario $name" >&2; exit 1 ;;
  esac
  bench "$name"
}

scenarios=("$@")
[ ${#scenarios[@]} -eq 0 ] && scenarios=(baseline loss2 loss5 delay100 cap3 cap1 capdrop uplink1)

{
  echo "| Scenario | Track | Received layer | fps | kbps | p50 ms | p99 ms | Freeze | Recovery |"
  echo "|---|---|---|---|---|---|---|---|---|"
  for s in "${scenarios[@]}"; do run "$s"; done
} | tee "results/results-$MODE.md"
unshape
$DC logs --no-color server > results/server.log 2>&1 || true
[ -n "${KEEP:-}" ] || $DC down >/dev/null 2>&1 || true
