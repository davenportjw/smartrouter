#!/bin/bash
# Gemini Smart Router - GKE Runner Fleet Deployer (CPU / GPU dynamic toggle)
set -e

RED='\033[0;31m'
GREEN='\033[0;32m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

log_info() {
    echo -e "${BLUE}[INFO] $1${NC}"
}

log_success() {
    echo -e "${GREEN}[SUCCESS] $1${NC}"
}

log_error() {
    echo -e "${RED}[ERROR] $1${NC}"
}

# 1. Validate GCP project context
PROJECT_ID=$(gcloud config get-value project 2>/dev/null || true)
if [ -z "$PROJECT_ID" ] || [ "$PROJECT_ID" = "(unset)" ]; then
    log_error "No active Google Cloud project found. Run 'gcloud config set project PROJECT_ID' first."
    exit 1
fi
log_info "Deploying to GCP Project: $PROJECT_ID"

# 2. Read dynamic configuration parameters (defaults or loaded from active environment)
CLUSTER_NAME="smartrouter-gke-cluster"
REGION="us-central1"
NAMESPACE="smartrouter"

# Resolve Cloud Run Control Plane Router URL dynamically
ROUTER_URL=$(gcloud run services describe gemini-smart-router --region "$REGION" --format="value(status.url)" --project="$PROJECT_ID" 2>/dev/null || true)
if [ -z "$ROUTER_URL" ]; then
    log_error "Failed to resolve active Cloud Run Router URL."
    exit 1
fi
log_info "Resolved Cloud Run Router URL: $ROUTER_URL"

# 3. Ask user for CPU vs GPU cost-saver choice or load from argument
CHOICE_ARG="$1"
if [ -z "$CHOICE_ARG" ]; then
    echo -e "\n------------------------------------------------------------"
    echo -e "   🚨 GKE RUNNER DEPLOYMENT COST-SAVER ACCELERATION TOGGLE  "
    echo -e "------------------------------------------------------------"
    echo -e "Select the computing tier for your GKE inference runners:"
    echo -e "  1) 🟢 GPU Mode (Standard Nvidia L4 Spot VMs - recommended for production)"
    echo -e "  2) 🔵 CPU-only Mode (Cost-Saver - runs Gemma 2B on CPU without GPU costs)"
    read -rp "Enter selection (1 or 2, default: 1): " CHOICE
else
    CHOICE="$CHOICE_ARG"
fi

MODE="gpu"
CPU_FLAG="false"
MEM_LIMIT="24Gi"
GPU_LIMIT="1"
NODE_POOL="spot-gpu-pool"
NODE_SELECTOR="cloud.google.com/gke-spot: \"true\"
        cloud.google.com/gke-accelerator: nvidia-l4"
ROLLOUT_TIMEOUT="360s"

if [ "$CHOICE" = "2" ]; then
    MODE="cpu"
    CPU_FLAG="true"
    MEM_LIMIT="4Gi"
    GPU_LIMIT="0"
    NODE_POOL="standard-cpu-pool"
    NODE_SELECTOR="cloud.google.com/gke-spot: \"true\""
    ROLLOUT_TIMEOUT="300s"
    log_info "🔵 cost-saver CPU-only mode selected!"
else
    log_info "🟢 GPU mode (Nvidia L4 Spot VMs) selected!"
fi

# 4. Build the headless runner container directly via Google Cloud Build (zero local Docker dependency)
IMAGE_TAG="gcr.io/$PROJECT_ID/smart-router-k8s-runner:latest"
if [ "$SKIP_BUILD" = "true" ]; then
    log_info "Skipping Google Cloud Build. Reusing precompiled container: $IMAGE_TAG"
else
    log_info "Building compute runner container image using Google Cloud Build: $IMAGE_TAG"
    # Write temporary Dockerfile in workspace context
    cat <<EOF > Dockerfile
FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o compute-runner cmd/runner/main.go

FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /root/
COPY --from=builder /app/compute-runner .
ENTRYPOINT ["./compute-runner"]
EOF

    gcloud builds submit --tag "$IMAGE_TAG" --project "$PROJECT_ID" .
    rm Dockerfile
fi

# 5. Connect GKE cluster context
log_info "Retrieving GKE cluster credentials..."
gcloud container clusters get-credentials "$CLUSTER_NAME" --region "$REGION" --project "$PROJECT_ID" || {
    log_error "GKE Cluster '$CLUSTER_NAME' not found. Creating cost-optimized Autopilot/Standard GKE cluster is required."
    exit 1
}

# 6. Generate tailored GKE Deployment yaml
log_info "Generating cost-optimized GKE deployment manifest..."

# Determine sidecar resources based on CPU/GPU choices
if [ "$MODE" = "gpu" ]; then
    SIDECAR_RESOURCES="limits:
            nvidia.com/gpu: \"1\"
            memory: $MEM_LIMIT
          requests:
            nvidia.com/gpu: \"1\"
            memory: 16Gi"
else
    SIDECAR_RESOURCES="limits:
            cpu: \"2\"
            memory: 4Gi
          requests:
            cpu: \"1\"
            memory: 2Gi"
fi

cat <<EOF > gke-spot-runner-run.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: smartrouter-runner-gke-spot
  namespace: $NAMESPACE
  labels:
    app: smartrouter-runner-spot
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
      # Target spot nodes
      nodeSelector:
        $NODE_SELECTOR
      tolerations:
      - key: "cloud.google.com/gke-spot"
        operator: "Equal"
        value: "true"
        effect: "NoSchedule"
      
      volumes:
      - name: model-volume
        emptyDir: {}
      
      initContainers:
      - name: model-downloader
        image: alpine:latest
        command: ["sh", "-c"]
        args:
        - |
          echo "Downloading lightweight Gemma 2B GGUF model weight..."
          apk add --no-cache wget unzip
          mkdir -p /models
          wget -O /models/model.gguf https://huggingface.co/lmstudio-community/gemma-2-2b-it-GGUF/resolve/main/gemma-2-2b-it-Q4_K_M.gguf
          
          echo "Downloading precompiled CPU llama-server binary..."
          wget -O /models/llama.zip https://github.com/ggerganov/llama.cpp/releases/download/b4719/llama-b4719-bin-ubuntu-x64.zip
          unzip -j /models/llama.zip "build/bin/*" -d /models/
          chmod +x /models/llama-* /models/rpc-server || true
          rm /models/llama.zip
          echo "Setup complete!"
        volumeMounts:
        - name: model-volume
          mountPath: /models

      containers:
      # 1. headless Go Poller Runner
      - name: poller-runner
        image: $IMAGE_TAG
        imagePullPolicy: Always
        args: [
          "--router-url=$ROUTER_URL",
          "--cluster-id=gke-$MODE-spot-pool",
          "--cpu-mode=$CPU_FLAG"
        ]
        resources:
          limits:
            cpu: "500m"
            memory: 2Gi
          requests:
            cpu: "250m"
            memory: 512Mi

      # 2. High-Performance llama.cpp Sidecar (serves locally on GKE pod localhost:8080)
      - name: llama-engine
        image: ubuntu:22.04
        command: ["sh", "-c"]
        args:
        - |
          apt-get update && apt-get install -y libcurl4 libgomp1 ca-certificates
          exec /models/llama-server --model /models/model.gguf --port 8080
        ports:
        - containerPort: 8080
        volumeMounts:
        - name: model-volume
          mountPath: /models
        env:
        - name: LD_LIBRARY_PATH
          value: "/models"
        resources:
          $SIDECAR_RESOURCES
EOF

# 7. Apply GKE configuration
log_info "Applying deployment to Kubernetes namespace '$NAMESPACE'..."
kubectl create namespace "$NAMESPACE" 2>/dev/null || true
kubectl apply -f gke-spot-runner-run.yaml --namespace "$NAMESPACE"
rm gke-spot-runner-run.yaml

log_info "Forcing rolling restart to download the fresh container updates..."
kubectl rollout restart deployment/smartrouter-runner-gke-spot --namespace="$NAMESPACE"
kubectl rollout status deployment/smartrouter-runner-gke-spot --namespace="$NAMESPACE" --timeout=$ROLLOUT_TIMEOUT

log_success "Dynamic local cluster runner fleet successfully deployed in $MODE mode!"
