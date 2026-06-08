# 🍏 Local Clusters Serving & Runner Integration Guide

Smart Router supports **Local Clusters**—a hybrid computing model that allows developer machines, server rooms, or Kubernetes node fleets to securely register local LLM serving capacity (running via `llama.cpp` / `llama-server` sidecars or local compilation layers) and dynamically process inference workloads using an asynchronous pull-and-resolve priority model.

---

## 🗺️ The Dynamic Pull Model Flow

Rather than requiring public IP addresses, port-forwarding, or firewall exceptions on computing edge nodes, Smart Router coordinates local compute resources using an active **pull model**:

1. **Client Request Suspended**: The client application submits a request to `/v1/models/gemma2:2b:generateContent`. Smart Router matching rules identify the location target as `local-cluster`, wrap the payload in a `QueueJob` with the app's priority, and hold the client connection open on a result channel.
2. **Runner Heartbeat & Polling**: Headless runner containers (running natively or deployed in Kubernetes) constantly poll `/api/v1/cluster/queue/poll` to retrieve outstanding jobs matching their `supported_models`.
3. **Local Inference Execution**: The runner forwards the request payload directly to the local high-performance `llama.cpp` `llama-server` sidecar endpoint on `http://localhost:8080/v1/chat/completions` (or `/completion`).
4. **REST Resolution**: Once complete, the runner posts the response body back to `/api/v1/cluster/queue/resolve`. Smart Router maps the `job_id`, delivers the payload to the pending client channel, and unblocks the HTTP response cleanly.

---

## 🚦 Core Control Plane REST API Specifications

Smart Router exposes these internal REST routes under the `/api/v1/cluster/...` namespace:

### 1. `POST /api/v1/cluster/runners/register`
Fired by dynamic runners on startup to announce serving specifications.
* **Payload**:
  ```json
  {
    "id": "runner-mac-1",
    "cluster_id": "local-cluster-alpha",
    "name": "Developer MBP (M3 Max)",
    "status": "online",
    "memory_allocated_gb": 64,
    "compute_gpu_cores": 40,
    "supported_models": ["gemma2:2b", "gemma2:2b-instruct-q4_K_M"],
    "max_model_size_gb": 32,
    "max_concurrent": 4
  }
  ```

### 2. `POST /api/v1/cluster/runners/heartbeat`
Fired periodically (every 2-5 seconds) by active runners to keep the status marked as `online` and report load metrics.
* **Payload**:
  ```json
  {
    "node_id": "runner-mac-1",
    "cluster_id": "local-cluster-alpha"
  }
  ```

### 3. `POST /api/v1/cluster/queue/poll`
Fired by runners to pull the next eligible high-priority job from the memory queue.
* **Payload**:
  ```json
  {
    "supported_models": ["gemma2:2b"]
  }
  ```
* **Responses**:
  - `204 No Content`: No eligible requests are currently in queue.
  - `200 OK`: Fills response with a `QueueJob` struct:
    ```json
    {
      "id": "job-1689043582000",
      "cluster_id": "local-cluster",
      "app_id": "app-production-billing",
      "model": "gemma2:2b",
      "priority": "high",
      "payload": "eyBwcm9tcHQ6ICJXaGF0IGlzIHNtYXJ0IHJvdXRpbmc/IiB9"
    }
    ```

### 4. `POST /api/v1/cluster/queue/resolve`
Fired by the runner to deliver the inference payload back to the router.
* **Payload**:
  ```json
  {
    "job_id": "job-1689043582000",
    "payload": "eyJyZXNwb25zZSI6ICJIZWxsbyBmcm9tIGxvY2FsIEdlbW1hIiB9",
    "status_code": 200
  }
  ```

---

## 🧪 Automated Testing & TDD Verification

To execute the unit and E2E integration tests for the local clusters queuing engine, execute standard Go testing tools:

```bash
# Run only local cluster queue tests
go test -v -run=TestCluster ./backend/proxy/...
go test -v -run=TestCluster ./backend/api/...

# Run runner engine loop tests
go test -v ./pkg/runner/engine/...

# Run all workspace tests to check for regressions
go test -v ./...
```

---

## ☸️ Kubernetes Runner (`smartrouter-runner-k8s`) - Server Fleet Setup

For high-throughput server rooms, dynamic container orchestrators leverage a **headless Go poller runner** communicating with a highly optimized inference backend like **llama.cpp** (`llama-server`) over a multi-container sidecar pattern.

```
+-------------------------------------------------------------------+
| Kubernetes Cluster Node                                           |
|  +--------------------------+            +---------------------+  |
|  | Container: Poller Runner |            | Container: Llama.cpp|  |
|  |  - Polls queue over REST | ---------> |  - serving GGUF     |  |
|  |  - Resolves outcomes     |            |  - localhost:8080   |  |
|  +--------------------------+            +---------------------+  |
+-------------------------------------------------------------------+
```

### 1. Multi-Stage Docker Pipeline (`Dockerfile.runner`)
The headless runner utilizes a lightweight scratch/alpine container:
```dockerfile
FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o runner cmd/runner/main.go

FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /root/
COPY --from=builder /app/runner .
ENTRYPOINT ["./runner"]
```

### 2. Kubernetes CPU-Optimized Runner Manifest
In CPU mode, GKE nodes download precompiled static x86_64 CPU `llama-server` binaries and satisfy OS dependencies dynamically:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: smartrouter-runner-gke-spot
  namespace: smartrouter
spec:
  replicas: 1
  template:
    spec:
      initContainers:
      - name: model-downloader
        image: alpine:latest
        command: ["sh", "-c"]
        args:
        - |
          # Download model weight
          wget -O /models/model.gguf https://huggingface.co/Qwen/Qwen2.5-1.5B-Instruct-GGUF/resolve/main/qwen2.5-1.5b-instruct-q4_k_m.gguf
          # Download static CPU-only llama-server build
          wget -O /models/llama.zip https://github.com/ggerganov/llama.cpp/releases/download/b4719/llama-b4719-bin-ubuntu-x64.zip
          unzip -j /models/llama.zip "build/bin/*" -d /models/
          chmod +x /models/llama-* || true
        volumeMounts:
        - name: model-volume
          mountPath: /models
      containers:
      - name: poller-runner
        image: gcr.io/YOUR_GCP_PROJECT_ID/smart-router-k8s-runner:latest
        args: ["--router-url=https://gemini-smart-router.a.run.app", "--cluster-id=gke-cpu-spot-pool", "--cpu-mode=true"]
      - name: llama-engine
        image: ubuntu:22.04
        command: ["sh", "-c"]
        args:
        - |
          apt-get update && apt-get install -y libcurl4 libgomp1 ca-certificates
          exec /models/llama-server --model /models/model.gguf --port 8080
        ports:
        - containerPort: 8080
        env:
        - name: LD_LIBRARY_PATH
          value: "/models"
        volumeMounts:
        - name: model-volume
          mountPath: /models
      volumes:
      - name: model-volume
        emptyDir: {}
```

---

## 🍏 E2E Verification Checklist

To manually verify the full E2E dynamic local clusters integration:

1. **Seed dynamic routing rules** to forward `gemma2:2b` requests to `local-cluster`.
2. **Start GKE or local runners**:
   ```bash
   # Launch CPU GKE fleet
   ./examples/local-cluster/deploy-runner.sh 2
   ```
3. **Submit a routed query**:
   ```bash
   curl -X POST "https://gemini-smart-router.a.run.app/v1/models/gemma2:2b:generateContent" \
     -H "Content-Type: application/json" \
     -H "x-goog-api-key: <YOUR_API_KEY>" \
     -d '{"contents": [{"role": "user", "parts": [{"text": "Hello local Gemma!"}]}]}'
   ```
