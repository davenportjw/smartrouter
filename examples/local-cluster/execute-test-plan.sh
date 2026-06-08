#!/bin/bash
# Gemini Smart Router - GKE Cost-Saver Cluster Deployment & Automated E2E Test Plan
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

# 1. Validate GCP Context
if [ -z "$PROJECT_ID" ]; then
    PROJECT_ID=$(gcloud config get-value project 2>/dev/null || true)
fi

if [ -z "$PROJECT_ID" ] || [ "$PROJECT_ID" = "(unset)" ]; then
    log_error "No active Google Cloud project found. Please set PROJECT_ID environment variable or run 'gcloud config set project PROJECT_ID' first."
    exit 1
fi
log_info "Setting active GCP Project context to: $PROJECT_ID"
gcloud config set project "$PROJECT_ID" --quiet

CLUSTER_NAME="smartrouter-gke-cluster"
REGION="us-central1"
NAMESPACE="smartrouter"

# 2. Secrets Management via Google Secret Manager
log_info "Verifying and loading secrets to Google Secret Manager..."
if [ ! -f .env ]; then
    log_info "Creating .env from template..."
    cp .env.sample .env
fi

# Generate BACKEND_SHARED_SECRET if empty
SHARED_SECRET=$(grep "^BACKEND_SHARED_SECRET=" .env | cut -d'="' -f2 | cut -d'"' -f1 || true)
if [ -z "$SHARED_SECRET" ]; then
    log_info "Generating secure shared secret..."
    SHARED_SECRET=$(LC_ALL=C tr -dc 'a-zA-Z0-9' < /dev/urandom | fold -w 32 | head -n 1)
    # Append/replace in .env
    if grep -q "^BACKEND_SHARED_SECRET=" .env; then
        sed -i.bak "s/^BACKEND_SHARED_SECRET=.*/BACKEND_SHARED_SECRET=\"$SHARED_SECRET\"/" .env && rm .env.bak
    else
        echo "BACKEND_SHARED_SECRET=\"$SHARED_SECRET\"" >> .env
    fi
fi

# Create and upload BACKEND_SHARED_SECRET to Secret Manager for zero-trust setups
if ! gcloud secrets describe "backend-shared-secret" --project="$PROJECT_ID" &>/dev/null; then
    log_info "Creating secret 'backend-shared-secret' in Secret Manager..."
    gcloud secrets create "backend-shared-secret" --replication-policy="automatic" --project="$PROJECT_ID"
fi
log_info "Uploading secret version..."
echo -n "$SHARED_SECRET" | gcloud secrets versions add "backend-shared-secret" --data-file=- --project="$PROJECT_ID"

# 3. Build & Deploy Control Plane (Cloud Run Serverless Hosting)
log_info "Deploying control plane Cloud Run Backend and Dashboard UI..."
./deploy.sh

BACKEND_URL=$(gcloud run services describe gemini-smart-router --region "$REGION" --format="value(status.url)" --project="$PROJECT_ID" 2>/dev/null || true)
if [ -z "$BACKEND_URL" ]; then
    log_error "Failed to retrieve Smart Router backend Cloud Run URL."
    exit 1
fi
log_info "Smart Router Backend active at: $BACKEND_URL"

# 4. Provision GKE Cost-Saver Node Pool (Standard Spot CPU pool to keep costs at ~$0)
log_info "Provisioning cost-saver GKE cluster node pool (Standard Spot CPU)..."
# Check if cluster exists
if ! gcloud container clusters describe "$CLUSTER_NAME" --region "$REGION" --project "$PROJECT_ID" &>/dev/null; then
    log_info "Creating GKE cluster '$CLUSTER_NAME' on GKE Autopilot (which automatically scales GCE spot nodes)..."
    gcloud container clusters create-auto "$CLUSTER_NAME" \
        --region "$REGION" \
        --project "$PROJECT_ID" \
        --quiet
fi

# 4.5 Configure Cloud Router and Cloud NAT for GKE outbound internet egress dynamically
log_info "Configuring Cloud Router and Cloud NAT for GKE outbound internet egress..."
if ! gcloud compute routers describe "smartrouter-gke-router" --region "$REGION" --project "$PROJECT_ID" &>/dev/null; then
    log_info "Creating Cloud Router 'smartrouter-gke-router' in default VPC network..."
    gcloud compute routers create "smartrouter-gke-router" \
        --network="default" \
        --region="$REGION" \
        --project="$PROJECT_ID"
fi

if ! gcloud compute routers nats describe "smartrouter-gke-nat" --router="smartrouter-gke-router" --region="$REGION" --project="$PROJECT_ID" &>/dev/null; then
    log_info "Creating Cloud NAT Gateway 'smartrouter-gke-nat'..."
    gcloud compute routers nats create "smartrouter-gke-nat" \
        --router="smartrouter-gke-router" \
        --region="$REGION" \
        --auto-allocate-nat-external-ips \
        --nat-all-subnet-ip-ranges \
        --project="$PROJECT_ID"
fi
log_success "Cloud NAT Gateway successfully configured! Outbound internet egress enabled for GKE Autopilot namespace."

# Fetch credentials
gcloud container clusters get-credentials "$CLUSTER_NAME" --region "$REGION" --project "$PROJECT_ID"

# 5. Dynamic Test Phase A: CPU Cost-Saver Mode Deployment
log_info "======================================================================"
log_info "🧪 TEST CYCLE 1/2: Deploying Compute Runner in CPU Cost-Saver Mode..."
log_info "======================================================================"
./examples/local-cluster/deploy-runner.sh "2"

# Seed rule routing a mock CPU model 'gemma2:2b' to local cluster
log_info "Seeding CPU test routing rule..."
curl -s -X POST "$BACKEND_URL/api/rules" \
    -H "X-Shared-Secret: $SHARED_SECRET" \
    -H "Content-Type: application/json" \
    -d "{
        \"id\": \"rule-test-cpu-runner\",
        \"model_pattern\": \"gemma2:2b\",
        \"client_tier\": \"all\",
        \"target_model\": \"gemma2:2b\",
        \"target_location\": \"local-cluster\",
        \"priority_weight\": 10
    }"

# Verify the GKE CPU runner registers dynamically and heartbeats successfully
log_info "Waiting 15 seconds for GKE CPU runner to register and heartbeat..."
sleep 15

# Check active registered nodes via admin endpoint
REG_RUNNERS_CPU=$(curl -s -X GET "$BACKEND_URL/api/v1/admin/runners" -H "X-Shared-Secret: $SHARED_SECRET")
log_info "Currently registered CPU runners: $REG_RUNNERS_CPU"

if [[ "$REG_RUNNERS_CPU" != *"gke-cpu-spot-pool"* ]]; then
    log_error "Verification Failed: GKE CPU Runner did not register successfully in GKE CPU Spot Pool."
    exit 1
fi
log_success "Test Cycle 1/2 Passed: CPU Cost-Saver Mode dynamically working!"

# Deregister/Cleanup CPU Runner deployment for GKE cleanly
log_info "Cleaning up CPU runner deployment..."
kubectl delete deployment smartrouter-runner-gke-spot --namespace="$NAMESPACE" || true

# 6. Dynamic Test Phase B: GPU Standard Spot Mode Deployment
log_info "======================================================================"
log_info "🧪 TEST CYCLE 2/2: Deploying Compute Runner in GPU Accelerated Mode..."
log_info "======================================================================"
./examples/local-cluster/deploy-runner.sh "1"

# Seed rule routing a mock GPU model 'llama3:8b' to local cluster
log_info "Seeding GPU test routing rule..."
curl -s -X POST "$BACKEND_URL/api/rules" \
    -H "X-Shared-Secret: $SHARED_SECRET" \
    -H "Content-Type: application/json" \
    -d "{
        \"id\": \"rule-test-gpu-runner\",
        \"model_pattern\": \"llama3:8b\",
        \"client_tier\": \"all\",
        \"target_model\": \"llama3:8b\",
        \"target_location\": \"local-cluster\",
        \"priority_weight\": 10
    }"

# Verify the GKE GPU runner registers dynamically and heartbeats successfully
log_info "Waiting 15 seconds for GKE GPU runner to register and heartbeat..."
sleep 15

# Check active registered nodes via admin endpoint
REG_RUNNERS_GPU=$(curl -s -X GET "$BACKEND_URL/api/v1/admin/runners" -H "X-Shared-Secret: $SHARED_SECRET")
log_info "Currently registered GPU runners: $REG_RUNNERS_GPU"

if [[ "$REG_RUNNERS_GPU" != *"gke-gpu-spot-pool"* ]]; then
    log_error "Verification Failed: GKE GPU Runner did not register successfully in GKE GPU Spot Pool."
    exit 1
fi
log_success "Test Cycle 2/2 Passed: GPU Accelerated Mode dynamically working!"

# Cleanup GKE Deployments
log_info "Cleaning up GKE testing resources..."
kubectl delete deployment smartrouter-runner-gke-spot --namespace="$NAMESPACE" || true

log_success "======================================================================"
log_success "🎉 ALL E2E LOCAL CLUSTER RUNNER TEST PLANS COMPLETED SUCCESSFULLY!"
log_success "Project Context: $PROJECT_ID"
log_success "======================================================================"
