#!/usr/bin/env bash
# End-to-end sanity test: hit nginx-test-svc /llama/api/generate, verify a
# non-empty response comes back, and surface the gateway's view of the
# scale-up so cold-start behaviour is observable.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
TEST_DIR="$REPO_ROOT/test"
NS="ollama-test"
LOCAL_PORT="${LOCAL_PORT:-8443}"
MODEL="${MODEL:-tinyllama}"
PROMPT="${PROMPT:-Reply with exactly one word: pong.}"
CURL_TIMEOUT="${CURL_TIMEOUT:-300}"

cleanup() {
    [[ -n "${PF_PID:-}" ]] && kill "$PF_PID" 2>/dev/null || true
}
trap cleanup EXIT

# Capture initial state for reporting.
INITIAL_REPLICAS=$(kubectl -n "$NS" get deploy ollama-worker -o jsonpath='{.spec.replicas}')
echo "ollama-worker replicas before request: $INITIAL_REPLICAS"

echo "=== Port-forward nginx-test-svc :443 -> localhost:$LOCAL_PORT ==="
kubectl -n "$NS" port-forward svc/nginx-test-svc "$LOCAL_PORT:443" >/tmp/ollama-test-pf.log 2>&1 &
PF_PID=$!

# Wait for the tunnel to be live.
for _ in $(seq 1 60); do
    if curl -sk --max-time 2 "https://localhost:$LOCAL_PORT/healthz" >/dev/null 2>&1; then
        break
    fi
    sleep 0.5
done
if ! curl -sk --max-time 2 "https://localhost:$LOCAL_PORT/healthz" >/dev/null 2>&1; then
    echo "FAIL: port-forward never became reachable"
    cat /tmp/ollama-test-pf.log
    exit 1
fi

REQUEST_BODY=$(jq -n --arg m "$MODEL" --arg p "$PROMPT" \
    '{model: $m, prompt: $p, stream: false}')

echo "=== POST /llama/api/generate (model=$MODEL) ==="
echo "Request: $REQUEST_BODY"

# Tail the gateway's go-proxy logs in the background so cold-start events are
# visible alongside the curl output.
kubectl -n "$NS" logs -l app=ollama-gateway -c go-proxy --tail=0 -f \
    > /tmp/ollama-test-gw.log 2>&1 &
LOG_PID=$!
trap 'cleanup; kill $LOG_PID 2>/dev/null || true' EXIT

START_NS=$(date +%s)
HTTP_STATUS=$(
    curl -sS -o /tmp/ollama-test-resp.json -w '%{http_code}' \
        --max-time "$CURL_TIMEOUT" \
        --cacert "$TEST_DIR/certs/ca.crt" \
        "https://localhost:$LOCAL_PORT/llama/api/generate" \
        -H 'Content-Type: application/json' \
        -d "$REQUEST_BODY" \
) || true
ELAPSED=$(( $(date +%s) - START_NS ))

# Stop log tail; give it a moment to flush.
kill $LOG_PID 2>/dev/null || true
sleep 0.5

echo
echo "=== HTTP $HTTP_STATUS in ${ELAPSED}s ==="
echo "--- Response ---"
if jq . /tmp/ollama-test-resp.json 2>/dev/null; then :; else cat /tmp/ollama-test-resp.json; fi
echo
echo "--- Gateway events during the request ---"
grep -E '"msg":"(scaling up|worker ready|http request|rate limited|scale-up failed|upstream proxy error)"' \
    /tmp/ollama-test-gw.log || echo "(no relevant events captured)"

echo
echo "--- Final state ---"
echo "ollama-worker replicas: $(kubectl -n "$NS" get deploy ollama-worker -o jsonpath='{.spec.replicas}')"

# Validate response.
if [[ "$HTTP_STATUS" != "200" ]]; then
    echo "FAIL: expected HTTP 200, got $HTTP_STATUS"
    exit 1
fi
RESP_TEXT=$(jq -r '.response // empty' /tmp/ollama-test-resp.json)
if [[ -z "$RESP_TEXT" ]]; then
    echo "FAIL: .response field is empty or missing"
    exit 1
fi

echo
echo "PASS: model returned ${#RESP_TEXT} chars in ${ELAPSED}s"
