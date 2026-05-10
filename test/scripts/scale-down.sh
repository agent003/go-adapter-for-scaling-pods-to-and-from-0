#!/usr/bin/env bash
# Force-trigger the cold-start path so test.sh exercises scale-from-zero
# without waiting IDLE_TIMEOUT for the idle monitor to fire.
#
# Steps:
#   1. Scale ollama-worker to 0 replicas and wait for the pod to terminate.
#   2. Restart the gateway so its in-memory "ready" cache is reset (the
#      gateway only clears that flag when *it* drives the scale-down; an
#      out-of-band kubectl scale would otherwise leave the cache stale and
#      the next request would 502 once before recovering).

set -euo pipefail

NS="ollama-test"

echo "=== Scaling ollama-worker to 0 replicas ==="
kubectl -n "$NS" scale deploy/ollama-worker --replicas=0
kubectl -n "$NS" wait --for=delete pod -l app=ollama-worker --timeout=60s 2>/dev/null || true

echo "=== Restarting gateway to flush its readiness cache ==="
kubectl -n "$NS" rollout restart deploy/ollama-gateway
kubectl -n "$NS" rollout status deploy/ollama-gateway --timeout=60s

echo
echo "Cold-start primed. Run: bash $(dirname "$0")/test.sh"
