# 🍏 Local Clusters Serving & Runner Integration Guide

Smart Router supports **Local Clusters**—a hybrid computing model that allows developer machines, server rooms, or Kubernetes node fleets to securely register local LLM serving capacity (running via `llama.cpp` / `llama-server` sidecars or local compilation layers) and dynamically process inference workloads using an asynchronous pull-and-resolve priority model.

---

## 🗺️ The Dynamic Pull Model Flow

Rather than requiring public IP addresses, port-forwarding, or firewall exceptions on computing edge nodes, Smart Router coordinates local compute resources using an active **pull model**:

1. **Client Request Suspended**: The client application submits a request to `/v1/models/gemma-4-4b:generateContent`. Smart Router matching rules identify the location target as `local-cluster`, wrap the payload in a `QueueJob` with the app's priority, and hold the client connection open on a result channel.
2. **Runner Heartbeat & Polling**: Headless runner containers (running natively or deployed in Kubernetes) constantly poll `/api/v1/cluster/queue/poll` to retrieve outstanding jobs matching their `supported_models`.
3. **Local Inference Execution**: The runner forwards the request payload directly to the local high-performance `llama.cpp` `llama-server` sidecar endpoint on `http://localhost:8080/v1/chat/completions` (or `/completion`).
4. **REST Resolution**: Once complete, the runner posts the response body back to `/api/v1/cluster/queue/resolve`. Smart Router maps the `job_id`, delivers the payload to the pending client channel, and unblocks the HTTP response cleanly.

---

## 🚦 Core Control Plane REST API Specifications

Smart Router exposes these internal REST routes under the `/api/v1/cluster/...` namespace:

### 1. `POST /api/v1/cluster/runners/register` (alias: `POST /v1/clusters/register`)
Fired by dynamic runners on startup (or via the dashboard's cURL join snippet) to announce serving specifications. Accepts an optional `?cluster_id=<id>` query parameter or `cluster_id` JSON field, and honors existing admin-configured runner overrides (`name`, `memory_allocated_gb`, `compute_gpu_cores`, `max_model_size_gb`, `max_concurrent`, `supported_models`, and `assigned_clusters`).
* **Payload**:
  ```json
  {
    "id": "runner-mac-1",
    "cluster_id": "local-cluster-alpha",
    "name": "Developer MBP (M3 Max)",
    "status": "online",
    "memory_allocated_gb": 64,
    "compute_gpu_cores": 40,
    "supported_models": ["gemma-4-4b", "gemma-4-26b"],
    "max_model_size_gb": 32,
    "max_concurrent": 4
  }
  ```

### 2. `POST /api/v1/cluster/runners/heartbeat`
Fired periodically (every 2-5 seconds) by active runners to keep the status marked as `online`, report load metrics, and receive live admin-configured overrides from the control plane.
* **Payload**:
  ```json
  {
    "node_id": "runner-mac-1",
    "cluster_id": "local-cluster-alpha"
  }
  ```
* **Response (`200 OK`)**: Returns the authoritative runtime configuration for the runner so the daemon dynamically updates its local `MaxModelSizeGB`, `MaxConcurrent`, `SupportedModels`, and `ClusterID` without restarting:
  ```json
  {
    "status": "ok",
    "max_model_size_gb": 32,
    "max_concurrent": 4,
    "supported_models": ["gemma-4-4b", "gemma-4-26b"],
    "assigned_clusters": ["local-cluster-alpha"]
  }
  ```

### 3. `POST /api/v1/cluster/queue/poll`
Fired by runners to pull the next eligible high-priority job from the memory queue. The control plane intersects the runner's `supported_models` with any admin-configured `SupportedModels`, filters out models whose estimated footprint exceeds `MaxModelSizeGB`, restricts jobs to the runner's `AssignedClusters` (blocking polling if the runner has been detached from all cluster pools), and enforces per-node `MaxConcurrent` in-flight job limits via `PollForNode`.
* **Payload**:
  ```json
  {
    "node_id": "runner-mac-1",
    "cluster_id": "local-cluster-alpha",
    "supported_models": ["gemma-4-4b", "gemma-4-26b"]
  }
  ```
* **Responses**:
  - `204 No Content`: No eligible requests are currently in queue, the runner is detached from all cluster pools, or the runner has reached its `MaxConcurrent` in-flight limit.
  - `200 OK`: Fills response with a `QueueJob` struct (tagged with `assigned_node_id`):
    ```json
    {
      "id": "job-1689043582000",
      "cluster_id": "local-cluster-alpha",
      "assigned_node_id": "runner-mac-1",
      "app_id": "app-production-billing",
      "model": "gemma-4-4b",
      "priority": "high",
      "payload": "eyBwcm9tcHQ6ICJXaGF0IGlzIHNtYXJ0IHJvdXRpbmc/IiB9"
    }
    ```

### 4. `POST /api/v1/cluster/queue/resolve`
Fired by the runner to deliver the inference payload back to the router and release the runner's in-flight concurrency slot.
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
FROM golang:1.26.10-alpine AS builder
WORKDIR /app
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o runner cmd/runner/main.go

FROM alpine:3.24
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
          # Download Gemma 4 GGUF model weight
          wget -O /models/model.gguf https://huggingface.co/google/gemma-4-4b-it-GGUF/resolve/main/gemma-4-4b-it-q4_k_m.gguf
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

## 🖥️ Managing Provider Clusters, Direct Endpoints & Runner Nodes (`/admin/clusters`)

The Compute Clusters dashboard (`/admin/clusters`) organizes hybrid and on-premises compute into **three distinct full-width CRUDL tables**:

1. **`1. On-Premises Clusters (Pull-Queue Pools)`**: Lists logical pull-queue pools (`Cluster Name & ID`, `Queue Name`, `Served Models` aggregated from attached nodes, `Max Queue Age (TTL)` with explicit failover status when served models are present, `Attached Nodes`, `Description`, `Actions`) with a **+ Add Cluster Pool** primary action (`hx-get="/admin/clusters/edit-modal?type=cluster"`).
2. **`2. Cluster Nodes (Managed Runners)`**: Live-polled every 5 seconds inside `#clusters-poller` (`GET /admin/clusters/active` and `GET /admin/clusters/poll`) with compact telemetry Intent Pills (`Central Queue: N Active Requests`, `N Online Nodes`, `Live polling · 5s`), a **+ Register Cluster Node** button (`hx-get="/admin/clusters/node-modal"`), 6 scannable columns (`Node & Heartbeat`, `Compute & Concurrency`, `Memory & Model Cap` with threshold bar, `Supported Models`, `Cluster Pool`, and `Actions`), and a `<tfoot>` fleet summary row showing online node count and central queue depth.
3. **`3. Custom / Direct HTTP Endpoints (Unmanaged Hardware)`**: Lists direct reverse-proxy endpoints (`Endpoint & Engine`, `Target URL`, `Served Model`, `Routing Role`, `Status`, `Actions`) with a **+ Add Direct Endpoint** primary action (`hx-get="/admin/clusters/edit-modal?type=endpoint"`).

### CRUDL Endpoints & Modals

* **Create or Edit Cluster Pool / Direct Endpoint Modal (`GET /admin/clusters/edit-modal?type=<cluster|pull-queue|endpoint|direct-http>[&id=<id>]`)**: Opens `#cluster-edit-modal` inside `#modal-container`. Omitting `id` opens **Create Mode** with editable ID inputs; providing `id` opens **Edit Mode** pre-populated with existing values.
* **Save Provider Cluster / Direct Endpoint (`POST /admin/clusters/providers/create` or `POST /admin/clusters/create`)**:
  * **Local Cluster (`type=cluster` or `type=pull-queue`)**: Creates or updates a `LocalCluster` (`ID`, `Name`, `QueueName`, `Description`, `MaxQueueAge`, `FallbackModel`, `FallbackLocation`) under `ProviderConfig.Regions["local-cluster"].LocalConfig.Clusters` while preserving attached runner `Nodes`. Routing rules can target either `"local-cluster"` or any configured `LocalCluster.ID` (e.g., `"cluster-m4-pro"`) as their `Location`. When `FallbackModel` is configured, queue timeouts or runner `>= 500` errors fail over to the specified cloud model and region with `X-Cluster-Fallback: true`; when left blank, true `504`/`502` errors are returned directly without silent fallback.
  * **Direct Endpoint (`type=endpoint` or `type=direct-http`)**: Creates or updates a `DirectEndpoint` (`ID`, `Name`, `ModelName`, `URL`, `Engine` (`vllm`/`ollama`/`sglang`/`mlx`/`llamacpp`/`contrastive-lm`), `Role` (`inference`/`decision_engine`), `ClusterID`, `Active`) under `ProviderConfig.Regions["local-cluster"].LocalConfig.DirectEndpoints`. Direct endpoints automatically translate between Gemini `:generateContent` and OpenAI `/v1/chat/completions` payloads.
* **Delete Provider Cluster or Endpoint (`DELETE /admin/clusters/providers/delete?type=<cluster|endpoint>&id=<id>`)**: Removes the cluster or direct endpoint from `LocalConfig`. Deleting a cluster pool also scrubs the deleted cluster ID from `AssignedClusters` across all registered runners.
* **Register or Configure Cluster Node Modal (`GET /admin/clusters/node-modal[?id=<runner_id>]` or `GET /admin/clusters/mappings-modal?id=<runner_id>`)**: Opens `#runner-mapping-modal` inside `#modal-container` (outside the 5-second `#clusters-poller` container). Supports **Pre-Registration (Create Mode)** when `id` is omitted (`status="pre-registered"`) and **Edit Mode** when `id` is provided, renders checkboxes for all configured `LocalCluster` pools plus any currently assigned clusters on the runner, and includes a copyable **Daemon / cURL Join Command** snippet dynamically wired to `PROXY_PUBLIC_URL` (`POST /api/v1/cluster/runners/register?cluster_id=<id>`, with `POST /api/v1/cluster/register` and `POST /v1/clusters/register` supported as aliases) and the runner's configured `supported_models`.
* **Save Cluster Node & Pool Assignments (`POST /admin/clusters/nodes/save`, `POST /admin/clusters/attach`, or `POST /admin/clusters/mappings/save`)**: Creates or updates a `RegisteredRunner` (`node_id`, `name`, `memory_allocated_gb`, `compute_gpu_cores`, `max_model_size_gb`, `max_concurrent`, `supported_models`, and `cluster_ids`). All changes take effect immediately at runtime:
  * Updating `cluster_ids` (including unchecking all clusters to detach a runner) persists `AssignedClusters` and synchronizes `LocalCluster.Nodes` without being overwritten by subsequent runner heartbeats or re-registrations.
  * Updating `supported_models` or `max_model_size_gb` restricts which queued models the runner can pull in `/api/v1/cluster/queue/poll` and propagates to the runner daemon on its next heartbeat.
  * Updating `max_concurrent` enforces a hard in-flight concurrency ceiling per runner node in both the control-plane queue (`PollForNode`) and the runner daemon's worker loop.
  * Runners whose `LastSeen` heartbeat is older than 30 seconds are automatically reported as `offline` by `/api/v1/admin/runners`.

---

## 🍏 E2E Verification Checklist

To manually verify the full E2E dynamic local clusters integration:

1. **Seed dynamic routing rules** to forward `gemma-4-4b` requests to `local-cluster` (or a named `LocalCluster` ID such as `mac-studio-pool`).
2. **Start GKE or local runners**:
   ```bash
   # Launch CPU GKE fleet
   ./examples/local-cluster/deploy-runner.sh 2
   ```
3. **Submit a routed query**:
   ```bash
   curl -X POST "https://gemini-smart-router.a.run.app/v1/models/gemma-4-4b:generateContent" \
     -H "Content-Type: application/json" \
     -H "x-goog-api-key: <YOUR_API_KEY>" \
     -d '{"contents": [{"role": "user", "parts": [{"text": "Hello local Gemma!"}]}]}'
   ```

