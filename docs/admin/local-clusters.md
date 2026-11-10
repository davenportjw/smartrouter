# 🍏 Local Clusters & Managed Runners Administration

The Compute Clusters dashboard (`/admin/clusters`) and Request Queue dashboard (`/admin/queue`) manage on-premises pull-queue pools, managed local runner daemons, and custom direct HTTP endpoints (Ollama, vLLM, SGLang, MLX, and `llama.cpp`).

---

## 1. On-Premises Clusters (`LocalCluster` Pull-Queue Pools)

On-premises cluster pools coordinate asynchronous pull-queue routing without requiring inbound firewall rules or public IP addresses on local developer machines or server rooms. Any cluster pool referenced by a connected runner node (`RegisteredRunner.AssignedClusters`) is also automatically discovered and surfaced in Table 1 (`1. On-Premises Clusters`) so operators can inspect, edit, or manage its attached nodes immediately.

### Creating or Editing a Cluster Pool & Attaching Existing Nodes
1. Navigate to **`/admin/clusters`** and click **+ Add Cluster Pool** (or click **+ Attach / Manage Nodes** or **Edit** on an existing pool row).
2. Configure the pool parameters and attached worker nodes inside the overlay modal (`#cluster-edit-modal`):
   - **Cluster Pool ID (Routing Target Location)**: Unique identifier (e.g., `mac-studio-pool`, `gke-cpu-spot-pool`). Routing rules can target either `local-cluster` or a specific `LocalCluster.ID` as their target `Location`.
   - **Display Name & Description**: Human-readable label and operational notes.
   - **Queue Name (`QueueName`)**: Logical pull-queue topic name (optional; defaults to `queue-<cluster-id>`).
   - **Max Queue Age / Timeout (`MaxQueueAge`)**: Per-cluster queue expiration window in seconds (defaults to `90s`). Queued requests waiting longer than `MaxQueueAge` expire automatically.
   - **Cloud Failover Model (`FallbackModel`) & Location (`FallbackLocation`)**: Optional user-specified cloud failover target (e.g., `gemini-3.8-flash` in `us-central1`).
     - **Blank (Default — Strict Error Mode)**: No silent cloud failover occurs (including on auto-discovered clusters). When local models are served by attached nodes, Table 1 displays `No failover (504 on timeout)`. If the cluster queue times out (`504 Gateway Timeout`) or a runner returns an upstream inference error (`502 Bad Gateway`), Smart Router surfaces the true error directly to the caller.
     - **Configured (User-Specified Cloud Failover)**: When a local cluster queue times out or returns a `>= 500` runner error, Smart Router forwards the original request to the configured `FallbackModel` and `FallbackLocation` and sets `X-Cluster-Fallback: true` on the HTTP response. In Table 1, the `Failover → <model>` badge is only shown alongside the cluster's `Served Models` once at least one attached node reports a primary served model.
   - **Attach Existing Cluster Nodes (`node_ids`)**: Select or deselect existing registered runner nodes directly within the **Add / Edit Cluster Pool** modal. Saving the cluster pool automatically updates both `LocalCluster.Nodes` and each runner's `AssignedClusters` list, and populates the pool's **Served Models** column in Table 1.

---

## 2. Managed Runner Nodes (`RegisteredRunner`)

Managed runners poll `/api/v1/cluster/queue/poll` over HTTPS and execute local inference via `llama-server` (`llama.cpp`) or compatible local sidecars.

### Pre-Registering or Configuring a Runner Node
1. Click **+ Register Cluster Node** (or click **Configure** on an existing runner row in **Cluster Nodes (Managed Runners)**).
2. Set the hardware and concurrency guardrails in `#runner-mapping-modal`:
   - **Runner Node ID & Friendly Name**: Unique node identifier (e.g., `runner-m4-max-01`) and display label.
   - **Allocated Memory (`memory_allocated_gb`) & GPU Cores (`compute_gpu_cores`)**: Reported or admin-overridden hardware capacity.
   - **Max Model Footprint (`max_model_size_gb`)**: Hard VRAM/RAM cap. The control plane filters out queued jobs whose estimated model size exceeds this threshold during `/api/v1/cluster/queue/poll`.
   - **Max Concurrent Jobs (`max_concurrent`)**: Hard per-node in-flight concurrency ceiling enforced both by `ClusterQueue.PollForNode` and the runner's local worker loop.
   - **Supported Models (`supported_models`)**: Comma-separated list of authorized models (e.g., `gemma-4-4b, gemma-4-26b`).
   - **Assigned Cluster Pools (`cluster_ids`)**: Checkboxes mapping the runner to one or more `LocalCluster` pools (`LocalCluster.Nodes` is automatically synchronized with `RegisteredRunner.AssignedClusters`).
3. **Detaching a Runner**: Unchecking all cluster pools detaches the runner (`AssignedClusters = []`). Detached runners are preserved across subsequent heartbeats and re-registrations and receive `204 No Content` on `/api/v1/cluster/queue/poll` until re-attached by an administrator.
4. **Heartbeat Staleness Detection**: Runners send heartbeats every 2–5 seconds to `/api/v1/cluster/runners/heartbeat`. Any runner whose `LastSeen` timestamp is older than **30 seconds** is automatically marked `offline` in `/api/v1/admin/runners` and `/admin/clusters`, while retaining its `LastSeen` timestamp for operator diagnostics.

---

## 3. Custom / Direct HTTP Endpoints (`DirectEndpoint`)

For self-hosted inference servers reachable directly by Smart Router over VPC or internal HTTP/HTTPS, register a **Direct Endpoint**:

1. Click **+ Add Direct Endpoint** on `/admin/clusters`.
2. Configure:
   - **Endpoint ID & Name**: Unique identifier (e.g., `vllm-gemma-26b`) and display name.
   - **Engine**: `ollama`, `vllm`, `sglang`, `mlx`, `llamacpp`, or `contrastive-lm`.
   - **Target URL**: Base URL of the inference server (e.g., `http://10.0.0.15:8000`).
   - **Served Model (`ModelName`)**: Target model name (e.g., `gemma-4-26b`).
   - **Routing Role**: `inference` (for generation workloads) or `decision_engine` (for complexity classification).
3. **Automatic Protocol Translation**:
   - Requests sent to `/v1/models/{model}:generateContent` (Gemini native format) or `/v1/chat/completions` (OpenAI format) are automatically translated into OpenAI `/v1/chat/completions` payloads for `ollama`, `vllm`, `sglang`, `mlx`, and `llamacpp` direct endpoints, and converted back to match the caller's requested response format (`X-Direct-Endpoint: <id>`).

---

## 4. Unified Priority Queue & Live Telemetry (`/admin/queue`)

The **Request Queue** view (`/admin/queue`) provides a unified real-time snapshot combining both upstream Vertex AI scheduler queues and on-premises `ClusterQueue` jobs:
- **Priority & Client Tier Ordering**: Queued jobs are ordered by application priority (`high` > `medium` > `low`) and client billing tier (`enterprise` > `premium` > `standard` > `free`), with FIFO ordering within the same priority and tier.
- **Queue / Runner Attribution**: Displays the target `ClusterID`, `AssignedNodeID` (for `in_flight` jobs actively executing on a runner), status (`queued` vs `in_flight`), and live active duration.
