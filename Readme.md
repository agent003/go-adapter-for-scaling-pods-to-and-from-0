# Scale-to-zero adapter for Ollama on Kubernetes

A small Go sidecar that lets you run [Ollama](https://ollama.com) on Kubernetes
with **true scale-to-zero**. Sits between an upstream proxy (e.g. nginx) and
the Ollama worker pod — scales the worker Deployment up on demand, holds the
incoming request in flight until the pod is `Ready`, forwards it, and scales
the Deployment back to zero after a configurable idle window. mTLS in transit
is terminated by Caddy sidecars co-located with the adapter and with Ollama.

## Why this exists

The Kubernetes Horizontal Pod Autoscaler (HPA) is the standard way to scale a
workload based on metrics, but it has a hard structural limitation: **HPA
cannot scale a Deployment from `0` to `1` based on incoming traffic** —
metrics-based scaling needs a running pod to produce metrics. For a stateful,
GPU-hungry workload like an Ollama-served LLM, paying to keep at least one
replica alive 24/7 is wasteful when traffic is bursty.

This project closes that gap with the smallest possible piece of code: a Go
proxy that talks to the Kubernetes API, patches `spec.replicas` on the worker
Deployment, and uses the inbound request itself as the scale-up signal. After
`IDLE_TIMEOUT` of silence, the same proxy patches replicas back to zero.

## Architecture

```
                                ┌──────────────────────────── Gateway Pod ─────┐
                                │                                              │
client ─https─▶  upstream  ─mTLS▶│   Caddy sidecar  ──localhost──▶  go-proxy   │
                proxy           │      :443                          :8080     │
              (e.g. nginx)      │                                       │      │
                                └───────────────────────────────────────┼──────┘
                                                                        │
                                                  Kubernetes API server │
                                                  (1) patch replicas=1  │
                                                  (2) poll ReadyReplicas│
                                                                        │
                                                                        ▼ https
                                ┌──────────────────────────── Worker Pod ──────┐
                                │                                              │
                                │   Caddy sidecar  ──localhost──▶  Ollama      │
                                │      :443                          127.0.0.1 │
                                │                                       :11434 │
                                │                                              │
                                │                              GGUF model files│
                                │                              (PVC mounted at │
                                │                               /root/.ollama) │
                                └──────────────────────────────────────────────┘

After IDLE_TIMEOUT of no incoming requests, the gateway patches the worker
Deployment to replicas=0. The pod terminates; the PVC retains the model so
the next cold-start does not re-download anything.
```

Caddy is a sidecar in **each** pod (gateway and worker) — not a single proxy
between them. Each Caddy terminates HTTPS for its own pod and forwards over
loopback to the application container behind it. This keeps the Ollama HTTP
listener bound to `127.0.0.1` only; the outside world only ever sees TLS.

## Implementation

### Cold-start handling (`go-proxy/scaler.go`)

When a request arrives and the local readiness flag is unset:

1. The first request takes a `sync.Mutex`, becomes the **leader**, and creates
   a shared `scaleState` channel. Subsequent concurrent requests find the
   non-`nil` channel, become **waiters**, and block on it.
2. The leader, in a separate goroutine bounded by `READY_TIMEOUT`,
   `MergePatch`es `spec.replicas` to `1` and then `Get`s the Deployment every
   `POLL_INTERVAL` until `status.ReadyReplicas > 0`.
3. On success the leader sets `ready=true` (an `atomic.Bool`), clears the
   shared state, and closes the channel — releasing every waiter at once.
4. Each handler then forwards via `httputil.NewSingleHostReverseProxy`. The
   Director is wrapped to set `req.Host = target.Host` so HTTPS upstreams that
   enforce strict SNI/Host matching (Caddy with `client_auth`, several other
   reverse proxies) do not reject the request.

The result: a 100-request burst against a cold gateway produces **one** K8s
patch and **one** poll loop, not 100 of each.

### Idle scale-down (`go-proxy/scaler.go`)

A background goroutine, started in `main` and bound to the root context:

- Every `IDLE_CHECK_INTERVAL`, reads the last-request timestamp from a small
  `ActivityTracker` (mutex-protected `time.Time`).
- If `time.Since(last) > IDLE_TIMEOUT`, it patches replicas to `0` and clears
  the readiness flag. If the patch fails, the timestamp is left intact so the
  next tick retries.

The reverse proxy's `ErrorHandler` also calls `MarkUnready()` on any 5xx-style
upstream error — so an out-of-band pod failure forces the next request to
re-verify rather than blindly forward to a dead listener.

### Operational features

- **Graceful shutdown**: `signal.NotifyContext` on SIGINT/SIGTERM, then
  `http.Server.Shutdown` with a configurable `SHUTDOWN_TIMEOUT`. The idle
  monitor honours the same context.
- **Structured logging**: JSON via `log/slog` to stdout; every HTTP request
  carries a `request_id` header (echoed from upstream or generated).
- **Health endpoints**: `/healthz` and `/readyz` (excluded from access logs
  and from rate-limiting).
- **Streaming-safe**: `proxy.FlushInterval = -1` so Ollama's line-delimited
  JSON streams reach the client without buffering. No write timeout on the
  HTTP server (write timeouts break long generations).
- **Rate limiting**: per-pod token bucket (`golang.org/x/time/rate`), used
  before the activity timestamp is touched so rate-limited callers cannot
  keep the worker alive.
- **Optional upstream TLS**: when the worker pod also fronts Ollama with a
  Caddy sidecar (the recommended pattern), `UPSTREAM_CA_CERT` and
  `UPSTREAM_SERVER_NAME` configure verification. `UPSTREAM_CLIENT_CERT` and
  `UPSTREAM_CLIENT_KEY` add mTLS to the inner hop if needed.
- **Hardened image**: distroless static + non-root, statically linked Go
  binary built with `-trimpath -s -w`, multi-arch via `BUILDPLATFORM`.

### RBAC

The gateway needs only the minimum to do its job. The Role in `k8s/rbac.yaml`
grants `get` and `patch` on a single named Deployment:

```yaml
- apiGroups: ["apps"]
  resources: ["deployments"]
  resourceNames: ["ollama-worker"]
  verbs: ["get", "patch"]
```

`list`, `watch`, and access to any other Deployment are intentionally not
granted. If you rename the worker, update both `DEPLOYMENT_NAME` in the
ConfigMap and `resourceNames` in the Role together.

## Configuration

All configuration is environment variables. Defaults are conservative; the
ConfigMap in `k8s/configmap.yaml` overrides them for production, and
`test/k8s/04-gateway-env.yaml` overrides them for the kind-based test.

| Variable | Default | Description |
| --- | --- | --- |
| `DEPLOYMENT_NAME` | `ollama-worker` | Worker Deployment to scale. Must match the `resourceNames` entry in the RBAC Role. |
| `NAMESPACE` | `default` | Namespace of the worker Deployment. |
| `OLLAMA_SERVICE_URL` | `https://ollama-worker-svc:443` | Upstream URL the proxy forwards to. |
| `IDLE_TIMEOUT` | `10m` | Duration of silence before the worker is scaled to zero. |
| `IDLE_CHECK_INTERVAL` | `30s` | How often the idle monitor checks the last-request timestamp. Must be `< IDLE_TIMEOUT`. |
| `READY_TIMEOUT` | `2m` | Maximum time to wait for `ReadyReplicas > 0` after a scale-up. |
| `POLL_INTERVAL` | `2s` | How often readiness is re-checked during cold-start. Must be `< READY_TIMEOUT`. |
| `RATE_LIMIT_RPS` | `5` | Token-bucket refill rate, requests per second per gateway pod. |
| `RATE_LIMIT_BURST` | `10` | Token-bucket burst size. |
| `LISTEN_ADDR` | `:8080` | HTTP listener for the proxy. The Caddy sidecar reverse-proxies to this on loopback. |
| `LOG_LEVEL` | `info` | One of `debug`, `info`, `warn`, `error`. |
| `SHUTDOWN_TIMEOUT` | `30s` | Grace period for in-flight requests on SIGTERM. |
| `UPSTREAM_CA_CERT` | *(empty)* | Path to a PEM CA bundle for verifying the worker's TLS certificate. Empty falls back to the system trust store. |
| `UPSTREAM_SERVER_NAME` | *(empty)* | Override SNI / `ServerName` for the upstream handshake. |
| `UPSTREAM_CLIENT_CERT` | *(empty)* | Optional client cert for mTLS to the upstream. |
| `UPSTREAM_CLIENT_KEY` | *(empty)* | Matching client key. Both must be set together. |
| `UPSTREAM_INSECURE_SKIP_VERIFY` | `false` | Disable upstream TLS verification. Development only. |

## Repository layout

```
.
├── go-proxy/                         The adapter (Go)
│   ├── main.go                       Entry, server lifecycle, handler, middleware, upstream transport
│   ├── config.go                     Environment parsing + validation
│   ├── scaler.go                     K8s scaler, activity tracker, idle monitor
│   ├── go.mod / go.sum
│   └── Dockerfile                    distroless/static + nonroot, multi-arch
├── caddy/                            mTLS sidecar
│   ├── Caddyfile
│   └── Dockerfile
├── k8s/                              Production manifests
│   ├── configmap.yaml
│   ├── deployment.yaml               Two-container pod: caddy-proxy + go-proxy
│   ├── rbac.yaml                     SA + Role scoped to a single named Deployment
│   └── service.yaml
├── test/                             Sanity-test harness for kind
│   ├── README.md                     Detailed test-suite documentation
│   ├── kind-config.yaml
│   ├── certs/                        openssl-based CA + multi-SAN server + client cert generator
│   ├── k8s/                          Namespaced manifests for nginx + gateway + worker test deployment
│   └── scripts/                      setup, test, scale-down, teardown
├── Readme.md
└── LICENSE                           Apache 2.0
```

## Deployment

Production deployment, in order:

1. **Generate certificates** (CA, server cert, client cert) and create the
   Kubernetes secret consumed by the Caddy sidecar:
   ```
   kubectl create secret generic ollama-mtls-certs \
     --from-file=ca.crt --from-file=tls.crt --from-file=tls.key
   ```
2. **Apply RBAC**: `kubectl apply -f k8s/rbac.yaml`.
3. **Build and push images** (replace `<your-registry>/...` placeholders in
   `k8s/deployment.yaml`):
   ```
   docker build -t <your-registry>/go-proxy:<tag> go-proxy
   docker build -t <your-registry>/caddy-sidecar:<tag> caddy
   docker push <your-registry>/go-proxy:<tag>
   docker push <your-registry>/caddy-sidecar:<tag>
   ```
4. **Apply the rest**:
   ```
   kubectl apply -f k8s/
   ```

The `ollama-worker` Deployment itself is not in this repo — it is assumed to
exist in the same namespace, with its own Caddy sidecar terminating HTTPS in
front of an Ollama container bound to `127.0.0.1:11434`.

## Test suite

A self-contained sanity-test harness lives in `test/`. It builds and exercises
the entire stack end-to-end on a single laptop using
[kind](https://kind.sigs.k8s.io/) (Kubernetes-in-Docker), with no external
services or registries.

### What is verified

| Behaviour | How it is checked |
| --- | --- |
| nginx serves HTTPS with a self-signed cert | `curl --cacert ca.crt https://localhost:8443/...` succeeds |
| nginx → gateway-caddy mTLS | otherwise gateway-caddy returns 400 before reaching go-proxy |
| Gateway holds the request during scale-from-zero | `duration_ms` of the request spans the scale-up window |
| Gateway K8s `Patch` and poll | gateway logs `"scaling up to 1 replica"` followed by `"worker ready" elapsed=…` |
| Gateway forwards to ollama-caddy over verified HTTPS | no TLS errors; Ollama returns 200 |
| ollama-caddy → ollama on loopback | `.response` field non-empty in the JSON |
| Idle scale-down | `kubectl get deploy ollama-worker` shows `replicas=0` after `IDLE_TIMEOUT` of silence |

### How to run

```
brew install docker kind kubectl openssl curl jq

bash test/scripts/setup.sh       # ~3-5 min first run; pre-pulls tinyllama
bash test/scripts/test.sh        # warm-path test against /llama/api/generate

# Cold-start path:
bash test/scripts/scale-down.sh  # scale worker to 0 + reset gateway readiness cache
bash test/scripts/test.sh

bash test/scripts/teardown.sh    # delete the kind cluster
```

The full procedure — including the cert recipe, the test-only
`IDLE_TIMEOUT=2m` override, and a troubleshooting table — is documented in
[`test/README.md`](test/README.md).

### Test-run results

Recorded on a single execution of the harness:

| Scenario | End-to-end latency | Notes |
| --- | --- | --- |
| Warm path (worker already at `replicas=1`) | **< 1 s** | `tinyllama` inference dominates |
| Cold start (worker at `replicas=0`) | **15.5 s** | Includes 14.0 s scale-up + ReadyReplicas wait |
| Idle scale-down | observed at **~2m20s** after last request | `IDLE_TIMEOUT=2m`, `IDLE_CHECK_INTERVAL=20s` |

Sample structured-log line emitted by the gateway during a cold-start
forward:

```json
{"level":"INFO","msg":"worker ready","component":"scaler","deployment":"ollama-worker","namespace":"ollama-test","elapsed":14023000000}
{"level":"INFO","msg":"http request","request_id":"ba3fcd86259f70d4","method":"POST","path":"/api/generate","status":200,"bytes":558,"duration_ms":15462}
```

### Memory and image footprint

Measured during the same test run, against the running gateway pod inside the
kind cluster (working-set RSS via `crictl stats`).

| Component | At idle (RSS) | Notes |
| --- | --- | --- |
| **`go-proxy`** | **~10 MiB** | The adapter |
| Caddy (gateway sidecar) | ~11 MiB | mTLS terminator |
| nginx (test ingress) | ~16 MiB | Plain pod, not Ingress |
| Ollama with `tinyllama` loaded | ~1.2 GiB | Model resident in RAM |

| Image / binary metric | Value |
| --- | --- |
| `ollama-go-proxy:test` Docker image (arm64) | **36.3 MB** |
| Embedded Go binary, statically linked, stripped | 33 MB (most is `client-go`) |
| Kubernetes `requests` for go-proxy | `cpu: 50m` / `memory: 64Mi` |
| Kubernetes `limits` for go-proxy | `cpu: 500m` / `memory: 256Mi` |

The 64 MiB request gives roughly six times headroom over the observed
working set, which leaves room for cold-start polling spikes and TLS
handshakes without paging.

### Test machine

| Property | Value |
| --- | --- |
| Hardware | Apple Mac M2 Pro (arm64) |
| OS | macOS Darwin 24.5.0 |
| Container runtime | Docker Desktop (containerd) |
| Cluster | kind v0.27.0, single control-plane node |
| Kubernetes | v1.34.x (kind default) |
| Go toolchain (build) | go 1.24.1 |
| Workload images | `caddy:2.7-alpine`, `nginx:1.25-alpine`, `ollama/ollama:latest` |
| Model | `tinyllama:1.1b` (~640 MB on disk) |

## License

[Apache License 2.0](LICENSE).
