# Sanity test setup

End-to-end test environment for the Ollama scale-to-zero adapter, running
inside a single-node `kind` (Kubernetes-in-Docker) cluster on a Mac M-series
laptop. All TLS material is self-signed; no external services or registries
are involved.

## Architecture

```
curl  ──►  nginx pod  (HTTPS :443, self-signed cert)
              │  /llama/* → upstream
              ▼
         gateway pod   ┌── caddy sidecar (mTLS :443) ──┐
                       └── go-proxy (HTTP :8080) ──────┘   ← the adapter
              │  HTTPS to upstream service
              ▼
       ollama-worker pod  ┌── caddy sidecar (HTTPS :443) ─┐
                          └── ollama (HTTP :11434) ──────┘
                                       │ localhost
                                       ▼
                                  llama model
```

- The **gateway** is the production code from `../go-proxy` and `../caddy`,
  reused here. Its caddy sidecar uses **mTLS** (the existing production
  Caddyfile pattern), so nginx must present a client certificate.
- The **worker** runs `ollama/ollama:latest` with a Caddy sidecar in front of
  it that terminates HTTPS and reverse-proxies to `localhost:11434` — the
  same shape as your real deployment.
- All certs are signed by a single self-signed CA. The server cert carries
  every relevant SAN so one cert covers all three services.

## Pre-requisites

Install once on the host:

```bash
brew install docker kind kubectl openssl curl jq
```

`docker info` must succeed (Docker Desktop running). Allocate at least 4GB
RAM to Docker Desktop — `tinyllama` runs fine, but headroom helps.

## Files

```
test/
├── README.md                  ← this file
├── kind-config.yaml           ← single-node kind cluster
├── certs/
│   └── generate-certs.sh      ← openssl: CA + multi-SAN server + client
├── k8s/                       ← namespace, RBAC, ConfigMaps, Deployments, Services
└── scripts/
    ├── setup.sh               ← full bootstrap (idempotent)
    ├── test.sh                ← end-to-end /llama/api/generate test
    ├── scale-down.sh          ← force a cold-start scenario
    └── teardown.sh            ← delete the kind cluster
```

## How the certs are generated

`certs/generate-certs.sh` writes three keypairs into `test/certs/`:

| File | Purpose |
| --- | --- |
| `ca.crt` / `ca.key` | Root CA (4096-bit RSA, 365 days, `CA:TRUE`) |
| `server.crt` / `server.key` | TLS server cert with SANs for nginx, gateway, worker, `localhost`, `127.0.0.1`. Used by all three caddy/nginx listeners. |
| `client.crt` / `client.key` | TLS client cert (`extendedKeyUsage = clientAuth`) used by nginx when calling the gateway. |

It's an openssl one-shot — three CSRs, three `openssl x509 -req -CA ...`
signs, no CA database needed. Re-running overwrites cleanly.

The certs are loaded into the cluster as a single `Secret` named
`ollama-test-certs` in the `ollama-test` namespace, created imperatively by
`setup.sh` (so private keys never sit in checked-in YAML). Pods mount only
the keys they need (e.g., the worker's caddy sidecar gets `server.crt` and
`server.key` but not `ca.crt`, since the worker doesn't verify clients).

To regenerate with a fresh CA:

```bash
rm -f test/certs/*.crt test/certs/*.key
bash test/certs/generate-certs.sh
# then re-create the Secret to roll the new material into the cluster:
kubectl -n ollama-test delete secret ollama-test-certs
bash test/scripts/setup.sh    # picks up the new certs and recreates everything that needs them
kubectl -n ollama-test rollout restart deploy/nginx-test deploy/ollama-gateway deploy/ollama-worker
```

## Bootstrap

```bash
bash test/scripts/setup.sh
```

This will, in order:

1. Verify `docker`, `kind`, `kubectl`, `openssl`, `curl`, `jq` are present.
2. Generate certs into `test/certs/` (skipped if they already exist).
3. Create the kind cluster `ollama-test` (skipped if it already exists).
4. `docker build` the `ollama-go-proxy:test` image from `../go-proxy`.
5. `kind load docker-image` for the proxy + stock images
   (`caddy:2.7-alpine`, `nginx:1.25-alpine`, `ollama/ollama:latest`) so
   nothing is pulled at apply time.
6. Apply all manifests, create the cert Secret.
7. Wait for `ollama-worker` to be Ready, then `kubectl exec ... ollama pull
   tinyllama` so cold-start tests don't include a download.
8. Wait for the gateway and nginx Deployments.

First run takes a few minutes (image pulls + model pull). Re-runs are fast.

Override the model with `MODEL=llama3.2:1b bash test/scripts/setup.sh` if
you want to test a slightly larger model. The default is `tinyllama` (~640
MB) which is the smallest llama variant on Ollama.

## Run the test

```bash
bash test/scripts/test.sh
```

The script:

1. Port-forwards `nginx-test-svc:443` to `localhost:8443`.
2. POSTs to `https://localhost:8443/llama/api/generate` with the test prompt
   (the cert SAN includes `localhost`, so curl's hostname check passes).
3. Tails `go-proxy` logs in parallel and prints the relevant events
   (`scaling up`, `worker ready`, `http request`).
4. Asserts HTTP 200 and a non-empty `.response`.
5. Prints the final replica count for visual confirmation.

Override knobs:

```bash
MODEL=tinyllama \
PROMPT="Reply with exactly one word: pong." \
LOCAL_PORT=8443 \
CURL_TIMEOUT=300 \
bash test/scripts/test.sh
```

## Test the cold-start path

After setup, ollama-worker is at `replicas=1` (from the model pre-pull). To
exercise the scale-from-zero behaviour:

```bash
bash test/scripts/scale-down.sh   # scale worker to 0 + restart gateway
bash test/scripts/test.sh         # next request triggers a cold start
```

The gateway's idle monitor will *also* scale the worker to 0 after
`IDLE_TIMEOUT=2m` of inactivity (the test ConfigMap shortens this from the
production default of 10m). Wait two minutes after the last request and the
next call exercises the same path naturally, no `scale-down.sh` needed.

## What the test verifies

| Behaviour | Surfaced by |
| --- | --- |
| nginx serves HTTPS with self-signed cert | curl handshake succeeds against `localhost:8443` |
| nginx mTLS to gateway-caddy | request gets past `mode require_and_verify`; otherwise 400 from caddy |
| Gateway holds the request, scales worker | gateway logs `"scaling up to 1 replica"` then `"worker ready"` before forwarding |
| Worker's caddy sidecar terminates TLS | `OLLAMA_SERVICE_URL=https://...:443` works; bad CA → 502 from go-proxy |
| Ollama responds | `.response` field non-empty in the curl JSON |
| Idle scale-down | `kubectl -n ollama-test get deploy ollama-worker` shows `replicas=0` ~2m after last request |

## Teardown

```bash
bash test/scripts/teardown.sh
```

Deletes the kind cluster (and all its data, including the ollama-models
PVC). Generated certs in `test/certs/` are kept; delete them by hand for a
clean slate.

## Troubleshooting

| Symptom | Likely cause |
| --- | --- |
| `setup.sh` hangs at `ollama pull` | First-run model download from registry.ollama.ai; can take 1–3 minutes for `tinyllama`, more for larger models. |
| `test.sh` returns HTTP 502 once after `scale-down.sh` | Shouldn't happen — `scale-down.sh` restarts the gateway to clear the readiness cache. If it does, the *next* request will succeed. |
| `test.sh` returns HTTP 400 from nginx | Client cert isn't being presented. Check `kubectl -n ollama-test logs deploy/ollama-gateway -c caddy` for an mTLS error. |
| `test.sh` returns HTTP 503 "scale-up timed out" | Worker pod failed to become Ready in `READY_TIMEOUT=3m`. Check `kubectl -n ollama-test describe pod -l app=ollama-worker` (most often: PVC stuck `Pending` or image pull stuck — neither expected after a successful `setup.sh`). |
| Gateway logs `x509: certificate signed by unknown authority` | The `UPSTREAM_CA_CERT` mount is missing or stale. Restart the gateway: `kubectl -n ollama-test rollout restart deploy/ollama-gateway`. |
