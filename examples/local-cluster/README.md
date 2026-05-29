# ☸️ Cost-Optimized GKE Spot GPU Deployment for Local Clusters

This example demonstrates how to deploy a Smart Router local serving runner inside **Google Kubernetes Engine (GKE) Autopilot** or GKE Standard using **Nvidia L4 Spot VMs** for maximum cost efficiency. 

By combining **GKE Spot VMs** with the pull-based serving architecture, you can achieve enterprise-grade LLM serving capacity (running **llama.cpp** or **vLLM**) with **up to 70% cost savings**, scaling completely to zero when the central Smart Router queue is empty.

---

## 💰 Cloud Cost Research & Optimization Strategy

When running hardware-accelerated LLM workloads in Google Cloud, select the compute engine based on your traffic profile to optimize costs:

| Computing Dimension | Option A: Google Cloud Run (GPU) | Option B: GKE Standard Spot VMs (L4) |
| :--- | :--- | :--- |
| **GPU Hardware** | Nvidia L4 (24GB VRAM) | Nvidia L4 (24GB VRAM) |
| **Pricing Model** | Pay-per-millisecond active execution | Hourly VM lease rate (billed per second) |
| **Hourly Rate (Active)**| **~$0.60/hour** (GPU + 4 vCPU + 16GB) | **~$0.12/hour** (GPU Spot VM rate) |
| **Idle Cost** | **$0.00** (scales to absolute zero) | **$0.00** (scales GKE nodes to zero via autoscaler) |
| **Cold Start Speed** | Slow (3 - 5 minutes to pull 10GB+ image) | Moderate (1 - 2 minutes to spin up Spot VM node) |
| **Best Suited For** | Intermittent, low-volume, or episodic workloads. | Continuous, batch, or high-volume workloads. |

### Key Takeaways:
1. **Spot VM Advantage**: Google Cloud Compute Engine **Spot VMs** offer Nvidia L4 GPUs at **~$0.11 - $0.12 per hour** (compared to the $0.35+ on-demand rate). 
2. **Graceful Preemption**: Because the Smart Router uses a pull-based queuing architecture, if a Spot VM is preempted/reclaimed by Google Cloud, the running runner simply drops the HTTP polling connection. The central Smart Router will safely hold the request in the priority queue and instantly dispatch it to the next available GKE pod replica, ensuring **zero request loss**.

---

## 🏗️ GKE Autopilot & Private Egress Architecture

When deploying serverless compute runners in GKE Autopilot clusters, you must account for VPC network constraints:

### 1. 🌐 Google Cloud NAT Outbound Egress Requirement
- **The Challenge**: By default, private GKE Autopilot namespaces run in closed subnets **without outbound internet egress**. The compute runner pods will fail to connect to your central Cloud Run router URL, hanging indefinitely and printing:
  `[Runner Warning] Registration POST failed: context deadline exceeded`.
- **The Solution**: A **Google Cloud Router** and **Google Cloud NAT Gateway** must be attached to GKE's VPC network. This enables stateless pods to establish outbound TCP connections to the internet (to poll the central queue) while remaining protected from unsolicited inbound traffic.
- **Automated Setup**: The orchestrator script ([execute-test-plan.sh](execute-test-plan.sh)) automatically provisions the Router and NAT Gateway dynamically:
  ```bash
  # Create Cloud Router
  gcloud compute routers create smartrouter-gke-router --network=default --region=us-central1
  # Create Cloud NAT Gateway
  gcloud compute routers nats create smartrouter-gke-nat --router=smartrouter-gke-router --region=us-central1 --auto-allocate-nat-external-ips --nat-all-subnet-ip-ranges
  ```

### 2. 🛡️ GKE Warden Webhook GPU Accelerator NodeSelectors
- **The Challenge**: GKE Autopilot uses admission controllers (Warden) to inspect pod resource shapes. If a pod requests discrete GPU cards (`nvidia.com/gpu: "1"`) but lacks specific hardware selectors, Warden rejects it:
  ` admission webhook denied the request: When requesting nvidia.com/gpu resources, you must specify cloud.google.com/gke-accelerator`.
- **The Solution**: All GPU manifests must explicitly declare the targeted accelerator card model:
  ```yaml
  nodeSelector:
    cloud.google.com/gke-spot: "true"
    cloud.google.com/gke-accelerator: nvidia-l4
  ```

---

## 🏗️ Deployment Manifests

### 1. GKE Spot VM Pod Spec (`gke-spot-runner.yaml`)
Deploy the Go runner poller container alongside a **llama.cpp sidecar** (or vLLM sidecar) using Spot VM node selectors and tolerations:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: smartrouter-runner-gke-spot
  namespace: smartrouter
spec:
  replicas: 1
  selector:
    matchLabels:
      app: smartrouter-runner-spot
  template:
    metadata:
      labels:
        app: smartrouter-runner-spot
    spec:
      # 1. Target GKE Spot Node Pool specifically
      nodeSelector:
        cloud.google.com/gke-spot: "true"
        cloud.google.com/gke-accelerator: nvidia-l4
      tolerations:
      - key: "cloud.google.com/gke-spot"
        operator: "Equal"
        value: "true"
        effect: "NoSchedule"
      
      volumes:
      # Shared cache volume for model weights
      - name: model-cache
        persistentVolumeClaim:
          claimName: model-cache-pvc
          
      containers:
      # 2. Core Headless Poller Runner
      - name: poller-runner
        image: gcr.io/my-project/smart-router-k8s-runner:latest
        imagePullPolicy: Always
        args: [
          "--router-url=https://gemini-smart-router-txgsracloq-uc.a.run.app",
          "--cluster-id=gke-gpu-spot-pool",
          "--cpu-mode=false"
        ]
        resources:
          limits:
            cpu: "1"
            memory: 2Gi
          requests:
            cpu: "250m"
            memory: 512Mi

      # 3. High-Performance llama.cpp Sidecar
      - name: llama-engine
        image: ghcr.io/ggerganov/llama.cpp:server
        args: [
          "--model", "/models/gemma-2-9b-it.gguf",
          "--port", "8080"
        ]
        ports:
        - containerPort: 8080
        resources:
          limits:
            nvidia.com/gpu: "1" # Allocates the L4 GPU
            memory: 24Gi
          requests:
            nvidia.com/gpu: "1"
            memory: 16Gi
```

---

## 🚀 How to Deploy & Verify

To provision the entire GKE Autopilot environment, deploy control plane portals, configure outbound NAT gateways, and run E2E verification tests:
```bash
chmod +x examples/local-cluster/execute-test-plan.sh
./examples/local-cluster/execute-test-plan.sh
```

To manually verify query routing through the local runner, execute:
```bash
chmod +x examples/local-cluster/run-local-query.sh
./examples/local-cluster/run-local-query.sh
```
