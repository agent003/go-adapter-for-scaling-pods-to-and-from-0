#!/usr/bin/env bash
# Bootstrap the kind-based sanity-test environment from scratch:
#   1. verify pre-reqs
#   2. generate self-signed certs (idempotent)
#   3. create the kind cluster (idempotent)
#   4. build the go-proxy image and load it + stock images into kind
#   5. apply manifests
#   6. pre-pull the tinyllama model so cold-start tests don't include a
#      multi-hundred-MB download
#
# Re-running is safe: missing pieces are filled in, existing ones are kept.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
TEST_DIR="$REPO_ROOT/test"
CERT_DIR="$TEST_DIR/certs"
K8S_DIR="$TEST_DIR/k8s"
CLUSTER_NAME="ollama-test"
NS="ollama-test"
MODEL="${MODEL:-tinyllama}"

step() { printf '\n=== %s ===\n' "$*"; }

step "Pre-req check"
missing=()
for cmd in docker kind kubectl openssl curl jq; do
    command -v "$cmd" >/dev/null || missing+=("$cmd")
done
if (( ${#missing[@]} > 0 )); then
    echo "Missing required tools: ${missing[*]}"
    echo "On macOS:   brew install ${missing[*]}"
    exit 1
fi
docker info >/dev/null 2>&1 || { echo "docker daemon not reachable; start Docker Desktop"; exit 1; }
echo "ok"

step "Generate self-signed certs"
if [[ -f "$CERT_DIR/ca.crt" && -f "$CERT_DIR/server.crt" && -f "$CERT_DIR/client.crt" ]]; then
    echo "certs already present in $CERT_DIR; skipping (delete them to regenerate)"
else
    bash "$CERT_DIR/generate-certs.sh"
fi

step "Create kind cluster: $CLUSTER_NAME"
if kind get clusters 2>/dev/null | grep -qx "$CLUSTER_NAME"; then
    echo "cluster $CLUSTER_NAME already exists"
else
    kind create cluster --name "$CLUSTER_NAME" --config "$TEST_DIR/kind-config.yaml"
fi
kubectl config use-context "kind-$CLUSTER_NAME"

step "Build go-proxy image"
docker build -t ollama-go-proxy:test "$REPO_ROOT/go-proxy"

step "Load images into kind"
kind load docker-image ollama-go-proxy:test --name "$CLUSTER_NAME"
for img in caddy:2.7-alpine nginx:1.25-alpine ollama/ollama:latest; do
    docker image inspect "$img" >/dev/null 2>&1 || docker pull "$img"
    kind load docker-image "$img" --name "$CLUSTER_NAME"
done

step "Apply namespace + RBAC + ConfigMaps"
kubectl apply -f "$K8S_DIR/00-namespace.yaml"
kubectl apply -f "$K8S_DIR/01-rbac.yaml"
kubectl apply -f "$K8S_DIR/02-ollama-caddyfile.yaml"
kubectl apply -f "$K8S_DIR/04-gateway-env.yaml"
kubectl apply -f "$K8S_DIR/05-gateway-caddyfile.yaml"
kubectl apply -f "$K8S_DIR/07-nginx-config.yaml"

step "Create cert secret (ollama-test-certs)"
kubectl -n "$NS" create secret generic ollama-test-certs \
    --from-file=ca.crt="$CERT_DIR/ca.crt" \
    --from-file=server.crt="$CERT_DIR/server.crt" \
    --from-file=server.key="$CERT_DIR/server.key" \
    --from-file=client.crt="$CERT_DIR/client.crt" \
    --from-file=client.key="$CERT_DIR/client.key" \
    --dry-run=client -o yaml | kubectl apply -f -

step "Apply Deployments + Services"
kubectl apply -f "$K8S_DIR/03-ollama-worker.yaml"
kubectl apply -f "$K8S_DIR/06-gateway.yaml"
kubectl apply -f "$K8S_DIR/08-nginx.yaml"

step "Wait for ollama-worker to become Ready"
kubectl -n "$NS" rollout status deploy/ollama-worker --timeout=300s

step "Pre-pull model: $MODEL"
POD=$(kubectl -n "$NS" get pod -l app=ollama-worker -o jsonpath='{.items[0].metadata.name}')
if kubectl -n "$NS" exec "$POD" -c ollama -- ollama list 2>/dev/null | awk 'NR>1 {print $1}' | grep -q "^${MODEL}\(:\|$\)"; then
    echo "model $MODEL already cached in PVC; skipping pull"
else
    echo "pulling $MODEL — first time can take several minutes"
    kubectl -n "$NS" exec "$POD" -c ollama -- ollama pull "$MODEL"
fi

step "Wait for gateway and nginx"
kubectl -n "$NS" rollout status deploy/ollama-gateway --timeout=180s
kubectl -n "$NS" rollout status deploy/nginx-test --timeout=120s

step "Setup complete"
cat <<EOF

Run a sanity test:
    bash $TEST_DIR/scripts/test.sh

Force a cold-start scenario (resets ollama replicas to 0 and the gateway's
ready cache), then re-run the test:
    bash $TEST_DIR/scripts/scale-down.sh
    bash $TEST_DIR/scripts/test.sh

Tail gateway logs:
    kubectl -n $NS logs -l app=ollama-gateway -c go-proxy -f

Teardown the whole cluster:
    bash $TEST_DIR/scripts/teardown.sh
EOF
