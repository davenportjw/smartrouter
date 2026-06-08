#!/bin/bash
# ==============================================================================
# ☸️ Smart Router - Local Cluster Inference Query Example
# ==============================================================================
# This script demonstrates how to submit a routed inference request that drains
# dynamically through your stateless GKE or macOS local compute runner pools.
# ==============================================================================

set -e

# Styling colors
RED='\033[0;31m'
GREEN='\033[0;32m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m'

log_info() { echo -e "${BLUE}[INFO] $1${NC}"; }
log_success() { echo -e "${GREEN}[SUCCESS] $1${NC}"; }
log_error() { echo -e "${RED}[ERROR] $1${NC}"; }
log_step() { echo -e "${CYAN}👉 $1${NC}"; }

# Load active settings
if [ -z "$PROJECT_ID" ]; then
    PROJECT_ID=$(gcloud config get-value project 2>/dev/null || true)
fi

if [ -z "$PROJECT_ID" ] || [ "$PROJECT_ID" = "(unset)" ]; then
    log_error "No active Google Cloud project found. Please set PROJECT_ID environment variable or run 'gcloud config set project PROJECT_ID' first."
    exit 1
fi
REGION="us-central1"

log_info "Fetching dynamic control plane configurations..."
BACKEND_URL=$(gcloud run services describe gemini-smart-router --region "$REGION" --format="value(status.url)" --project="$PROJECT_ID" 2>/dev/null || true)
if [ -z "$BACKEND_URL" ]; then
    log_error "Unable to resolve active Cloud Run Backend URL. Deploy control plane first."
    exit 1
fi

SHARED_SECRET=$(gcloud secrets versions access latest --secret="backend-shared-secret" --project="$PROJECT_ID" 2>/dev/null || true)
if [ -z "$SHARED_SECRET" ]; then
    log_error "Unable to retrieve shared secret from Secret Manager."
    exit 1
fi

# Fetch OIDC identity token for Google Cloud Run IAM authentication
log_info "Generating OIDC Token for Cloud Run gateway..."
OIDC_TOKEN=$(gcloud auth print-identity-token --audiences="$BACKEND_URL" --project="$PROJECT_ID" 2>/dev/null || true)

# Define verification parameters
CLIENT_API_KEY="gr_local_cluster_verify_key"
APP_ID="app-local-verify"
CLIENT_ID="client-local-verify"
RULE_ID="rule-route-to-gke-cpu"

# ==============================================================================
# 1. Seed Local Configuration Mappings
# ==============================================================================
log_info "Seeding local cluster configuration mappings in database..."

# Seed Client
curl -s -X POST "$BACKEND_URL/api/clients" \
    -H "Authorization: Bearer $OIDC_TOKEN" \
    -H "X-Shared-Secret: $SHARED_SECRET" \
    -H "Content-Type: application/json" \
    -d "{
        \"id\": \"$CLIENT_ID\",
        \"name\": \"Local GKE Verification Client\",
        \"tier\": \"premium\",
        \"rpm\": 60,
        \"tpm\": 40000,
        \"priority\": \"high\"
    }"

# Seed App
curl -s -X POST "$BACKEND_URL/api/apps" \
    -H "Authorization: Bearer $OIDC_TOKEN" \
    -H "X-Shared-Secret: $SHARED_SECRET" \
    -H "Content-Type: application/json" \
    -d "{
        \"id\": \"$APP_ID\",
        \"client_id\": \"$CLIENT_ID\",
        \"name\": \"GKE Local Runner App\",
        \"rpm\": 60,
        \"tpm\": 40000,
        \"priority\": \"high\"
    }"

# Seed API Key (Hashing dynamically for document mapping)
KEY_HASH=$(echo -n "$CLIENT_API_KEY" | shasum -a 256 | cut -d' ' -f1)
curl -s -X POST "$BACKEND_URL/api/keys" \
    -H "Authorization: Bearer $OIDC_TOKEN" \
    -H "X-Shared-Secret: $SHARED_SECRET" \
    -H "Content-Type: application/json" \
    -d "{
        \"key_hash\": \"$KEY_HASH\",
        \"client_id\": \"$CLIENT_ID\",
        \"app_id\": \"$APP_ID\",
        \"status\": \"active\"
    }"

# Seed High-Priority Local Routing Rule (Targeting local GKE CPU Pool)
log_info "Seeding rule to route model 'gemma2:2b' to GKE CPU Spot Pool..."
curl -s -X POST "$BACKEND_URL/api/rules" \
    -H "Authorization: Bearer $OIDC_TOKEN" \
    -H "X-Shared-Secret: $SHARED_SECRET" \
    -H "Content-Type: application/json" \
    -d "{
        \"id\": \"$RULE_ID\",
        \"app_id\": \"$APP_ID\",
        \"model_pattern\": \"gemma2:2b\",
        \"client_tier\": \"premium\",
        \"target_model\": \"gemma2:2b\",
        \"target_location\": \"local-cluster\",
        \"priority_weight\": 100
    }"

log_success "Configuration database seeded successfully!"
echo -e "------------------------------------------------------------"
echo -e "✨ READY TO INITIATE LOCAL ROUTING CYCLE"
echo -e "------------------------------------------------------------"
log_step "Step 1: Run your GKE CPU runner using:"
echo -e "        ./examples/local-cluster/deploy-runner.sh 2"
log_step "Step 2: Submit your routed query targeting 'gemma2:2b' using:"
echo -e "        curl -X POST \"$BACKEND_URL/v1/models/gemma2:2b:generateContent\" \\"
echo -e "            -H \"x-goog-api-key: $CLIENT_API_KEY\" \\"
echo -e "            -H \"X-Client-App-ID: $APP_ID\" \\"
echo -e "            -d '{\"contents\": [{\"parts\": [{\"text\": \"Hello local Gemma!\"}]}]}'"
echo -e "------------------------------------------------------------"
log_step "Step 3: Cleanup configuration database when done by running:"
echo -e "        ./examples/local-cluster/run-local-query.sh --cleanup"
echo -e "------------------------------------------------------------"

# Cleanup logic check
if [ "$1" = "--cleanup" ]; then
    log_info "Cleaning up temporary local database seeds..."
    
    # Dynamically hash API Key to locate doc ID
    KEY_HASH=$(echo -n "$CLIENT_API_KEY" | shasum -a 256 | cut -d' ' -f1)
    
    # Revoke/Delete API Key passing hash in query parameter
    curl -s -X POST "$BACKEND_URL/api/keys/revoke?hash=$KEY_HASH" \
        -H "Authorization: Bearer $OIDC_TOKEN" \
        -H "X-Shared-Secret: $SHARED_SECRET"
        
    # Delete Rule
    curl -s -X DELETE "$BACKEND_URL/api/rules?id=$RULE_ID" \
        -H "Authorization: Bearer $OIDC_TOKEN" \
        -H "X-Shared-Secret: $SHARED_SECRET"
        
    log_success "Cleanup finished successfully!"
fi
