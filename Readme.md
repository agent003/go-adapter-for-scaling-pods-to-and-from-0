# 🚀 Ollama Auto-Scaler & mTLS Proxy

This project implements a secure, cost-efficient gateway for Ollama. It provides a hardened entry point that only allows authorized mTLS traffic and automatically manages the lifecycle of your Ollama worker—scaling it up on demand and scaling it back to zero when idle.

## 🏗️ Architecture Overview

The system uses a **Sidecar Pattern** within a single Kubernetes Pod to separate security from logic:

1. **Caddy (Port 443):** The "Shield." It handles mTLS termination, ensuring only clients with a valid certificate can even talk to the proxy.
2. **Go Proxy (Port 8080):** The "Brain." It manages the Kubernetes API calls to scale your worker deployment, waits for readiness, and handles rate limiting.
3. **Ollama Worker:** The "Muscle." Your actual Ollama deployment that runs the LLMs.

## 🛠️ Implementation Details

### 1. Scale-to-Zero (Economy Mode)

The Go proxy runs a background **Inactivity Monitor**. If no requests are received within the `IDLE_TIMEOUT` period (configured in the ConfigMap), the proxy automatically patches the Ollama worker deployment to **0 replicas**.

### 2. Cold-Start Readiness Polling

When a request hits the gateway while the worker is at 0 replicas:

* The proxy triggers a scale-up to **1 replica**.
* Instead of failing the request, it holds it in an **inflight state**.
* It polls the Kubernetes API until `ReadyReplicas > 0`.
* Once the pod is ready, it forwards the request.

### 3. Rate Limiting

To prevent a single client from overwhelming the GPU/CPU, a **Token Bucket rate limiter** is implemented. Both RPS (Requests Per Second) and Burst limits are fully configurable via environment variables.

## 📁 Repository Structure

```text
ollama-gateway/
├── caddy/
│   ├── Caddyfile        # Proxy & mTLS rules
│   └── Dockerfile       # Lightweight Caddy build
├── go-proxy/
│   ├── main.go          # Scaling, Polling & Proxy logic
│   ├── go.mod           # Go dependencies
│   └── Dockerfile       # Multi-stage minimal build
├── k8s/
│   ├── configmap.yaml   # Centralized configuration
│   ├── rbac.yaml        # Permissions for the Go Proxy
│   ├── deployment.yaml  # Multi-container Pod spec
│   └── service.yaml     # Internal ClusterIP
└── README.md            # This file

```

## ⚙️ Configuration Parameters

| Variable | Description | Default |
| --- | --- | --- |
| `DEPLOYMENT_NAME` | The name of the Ollama worker deployment to scale. | `ollama-worker` |
| `IDLE_TIMEOUT` | Time of silence before scaling to zero (e.g., `5m`, `1h`). | `10m` |
| `READY_TIMEOUT` | Max time to wait for a pod to boot before timing out. | `2m` |
| `RATE_LIMIT_RPS` | Allowed requests per second. | `5` |
| `OLLAMA_SERVICE_URL` | The internal K8s address of the worker service. | `http://...` |

## 🚀 Deployment Steps

1. **Generate Certificates:** Create your CA and client/server certs.
2. **K8s Secret:** Create a secret named `ollama-mtls-certs` containing `ca.crt`, `tls.crt`, and `tls.key`.
3. **Apply RBAC:** `kubectl apply -f k8s/rbac.yaml`.
4. **Build Images:** Build your Caddy and Go Dockerfiles and push them to your registry.
5. **Deploy:** Update the image names in `k8s/deployment.yaml` and apply all manifests in the `k8s/` folder.

---

