#!/usr/bin/env bash
# Phase 1 load test on Linux: server and load generator in separate
# containers pinned to separate CPUs (Docker's Linux VM on a Mac, or a Linux
# host). Prints server CPU (100% = one core) next to the loadtest summary.
# Usage: netem/loadtest.sh [loadtest flags]   e.g. -rooms 4 -n 6 -sub-layer 2
set -euo pipefail
cd "$(dirname "$0")"
out=$(mktemp)
SRV_CPUS=${SRV_CPUS:-0-1}
GEN_CPUS=${GEN_CPUS:-2-7}
docker build -q -t quicvideo-netem -f Dockerfile .. >/dev/null
docker network create qv-load >/dev/null 2>&1 || true
docker rm -f qv-load-server >/dev/null 2>&1 || true
# High initial estimate: every subscriber gets its max layer at once (this
# measures forwarding cost, not the controller).
docker run -d --name qv-load-server --network qv-load --cpuset-cpus "$SRV_CPUS" \
  -e AUTH_INSECURE=true -e ENV=production -e INITIAL_BITRATE=1000000000 \
  quicvideo-netem quicvideod serve >/dev/null
sleep 2
docker run --rm --network qv-load --cpuset-cpus "$GEN_CPUS" quicvideo-netem \
  loadtest -addr qv-load-server:7710 -insecure -warmup 4s -duration 20s "$@" > "$out" 2>&1 &
gen=$!
sleep 6
samples=()
for _ in $(seq 8); do
  samples+=("$(docker stats --no-stream --format '{{.CPUPerc}}' qv-load-server | tr -d %)")
  sleep 1
done
wait $gen
echo "server cpu % samples: ${samples[*]}"
cat "$out"; rm -f "$out"
docker rm -f qv-load-server >/dev/null
