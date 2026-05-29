# 🧪 Local Runners E2E Testing Plan

This document outlines the comprehensive test plan, setup prerequisites, execution steps, and cleanup protocols for verifying the **Local Runners & Computing Clusters** architecture within the Smart Router ecosystem.

---

## 🗺️ Test Architecture Overview

The local runner test suite covers three critical verification layers:

```
+-----------------------------------------------------------------+
|  Layer 1: In-Memory Queue Unit Verification (Fast)             |
|  - Validates priority weight matching, sorting, & TTL reapers.   |
+-----------------------------------------------------------------+
                                |
                                v
+-----------------------------------------------------------------+
|  Layer 2: Cost-Saver GKE CPU Runner E2E (Validation)            |
|  - Downloads static x86_64 CPU llama-server package in GKE.     |
|  - Mounts unprivileged ubuntu sidecars with libcurl/libgomp.     |
+-----------------------------------------------------------------+
                                |
                                v
+-----------------------------------------------------------------+
|  Layer 3: High-Performance GKE GPU Runner E2E (Production)      |
|  - Allocates Nvidia L4 accelerators to GKE Spot instances.      |
|  - Spins up official CUDA-optimized llama.cpp image targets.    |
+-----------------------------------------------------------------+
```

---

## 🏁 Layer 1: In-Memory Local Queue Unit Verification

These tests execute locally without any external Kubernetes or Google Cloud dependencies, validating the thread-safety, sorting, and client-channel blocking/unblocking mechanics of the memory queues.

### Execution Command
Run only the local cluster-specific test suites using standard Go matching flags:
```bash
# Test cluster registry and REST endpoints in API layer
go test -v -run=TestCluster ./backend/api/...

# Test priority queues, TTL reapers, and channel blocks in proxy layer
go test -v -run=TestCluster ./backend/proxy/...

# Test headless runner engine client execution loops
go test -v ./pkg/runner/engine/...
```

---

## 🔵 Layer 2: Cost-Saver GKE CPU Runner E2E Verification

This plan verifies unaccelerated CPU serving using standard x86_64 cores. It downloads a lightweight `Qwen 1.5B` GGUF model and a precompiled static `llama-server` binary to run unprivileged on GKE CPU spot pools.

### Prerequisites
* Active GCP Project (`davenport-boutique` or target sandbox).
* Valid GKE Autopilot Cluster initialized.
* Target router deployed to Google Cloud Run.

### Step-by-Step Execution

#### 1. Seed Configurations in Database
Configure Firestore mapping rules to redirect requests targeting `gemma2:2b` to our dynamic local cluster queue:
```bash
./examples/local-cluster/run-local-query.sh
```

#### 2. Deploy CPU Spot Runners to GKE
Deploy the Go poller runner and dynamic unprivileged CPU `llama-server` sidecars using option `2`:
```bash
SKIP_BUILD=true ./examples/local-cluster/deploy-runner.sh 2
```

#### 3. Monitor Rollout & Heartbeat Status
Verify GKE pod state enters `Running` (meaning `model-downloader` completed downloads and sidecars booted successfully):
```bash
# Get running pod names
kubectl get pods -n smartrouter

# Check registered runners online inside the control plane
SECRET=$(gcloud secrets versions access latest --secret="backend-shared-secret" --project="YOUR_PROJECT")
curl -s -H "X-Shared-Secret: $SECRET" "https://<YOUR_ROUTER_URL>/api/v1/admin/runners"
```

#### 4. Submit E2E routed Prompt Query
Submit a generated content request targeting `gemma2:2b` via the router:
```bash
curl -s -X POST "https://<YOUR_ROUTER_URL>/v1/models/gemma2:2b:generateContent" \
    -H "Content-Type: application/json" \
    -H "x-goog-api-key: gr_local_cluster_verify_key" \
    -H "X-Client-App-ID: app-local-verify" \
    -d '{"contents": [{"role": "user", "parts": [{"text": "What is 2 + 2? Answer in exactly one word."}]}]}'
```
*   **Expected Result**: `{"candidates":[{"content":{"parts":[{"text":"4"}]}}]}`

#### 5. Verify logs Natively processed
Inspect the `poller-runner` and `llama-engine` logs to confirm the request was resolved locally inside the cluster and **did not fall back** to Vertex AI:
```bash
# 1. Check poller-runner logs for pulled job logs (must NOT show backup warning):
kubectl logs deployment/smartrouter-runner-gke-spot -c poller-runner -n smartrouter --tail=30

# 2. Check llama-engine sidecar logs to see prompt timing:
kubectl logs deployment/smartrouter-runner-gke-spot -c llama-engine -n smartrouter --tail=30
```

---

## 🟢 Layer 3: High-Performance GKE GPU Runner E2E Verification

This plan verifies full hardware-accelerated serving using physical Nvidia L4 GPU spot VM resources, running official containerized `llama-server` tags.

### Step-by-Step Execution

#### 1. Deploy GPU Spot Runners to GKE
Deploy the runner utilizing Nvidia L4 Spot allocations by selecting option `1`:
```bash
SKIP_BUILD=true ./examples/local-cluster/deploy-runner.sh 1
```
*   **GKE Autopilot Action**: Dynamically requests node sizes mounting `nvidia.com/gpu: "1"`. GKE Autopilot automatically provisions standard `spot-gpu-pool` instances and configures GPU drivers.

#### 2. Submit E2E Routed GPU Prompt Query
Submit a content generation prompt via the Cloud Run router proxy:
```bash
curl -s -X POST "https://<YOUR_ROUTER_URL>/v1/models/gemma2:2b:generateContent" \
    -H "Content-Type: application/json" \
    -H "x-goog-api-key: gr_local_cluster_verify_key" \
    -H "X-Client-App-ID: app-local-verify" \
    -d '{"contents": [{"role": "user", "parts": [{"text": "What is the chemical symbol for water? Answer in exactly one word."}]}]}'
```
*   **Expected Result**: `{"candidates":[{"content":{"parts":[{"text":"H2O"}]}}]}`

#### 3. Verify CUDA acceleration
Confirm model evaluation is accelerated by Nvidia GPUs using container metrics:
```bash
# Check that CUDA kernels compiled and ran prompt evaluation:
kubectl logs deployment/smartrouter-runner-gke-spot -c llama-engine -n smartrouter --tail=50
```

---

## 🧹 Teardown & Cost Prevention Protocols

> [!CAUTION]
> Dynamic spot VMs and GPU allocations in GKE incur ongoing compute charges. Always clean up every test immediately upon completion to avoid cost leakage.

Execute this cleanup sequence immediately following any testing iteration:

```bash
# 1. Delete GKE Pod deployments from the cluster namespace
kubectl delete deployment smartrouter-runner-gke-spot -n smartrouter

# 2. Revoke all Firestore routing seeds and temporary client configs
./examples/local-cluster/run-local-query.sh --cleanup
```
