#!/usr/bin/env bash
set -euo pipefail

# Console Colors
BLUE='\033[0;34m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m' # No Color

echo -e "${BLUE}=======================================================${NC}"
echo -e "${BLUE}🚀 Deploying Gemini API Key Client to Cloud Run...    ${NC}"
echo -e "${BLUE}=======================================================${NC}"

# 1. Detect Active GCP Project
PROJECT_ID=$(gcloud config get-value project 2>/dev/null || true)
if [ -z "$PROJECT_ID" ]; then
  echo -e "${RED}❌ Error: No active Google Cloud project set in gcloud config.${NC}"
  echo -e "Run: ${YELLOW}gcloud config set project YOUR_PROJECT_ID${NC}"
  exit 1
fi
echo -e "${GREEN}✓ Active GCP Project: ${YELLOW}$PROJECT_ID${NC}"

# 2. Configure defaults
SERVICE_NAME="gemini-apikey-client"
REGION="us-central1"

# 3. Ask or read environment configurations
if [ -z "${ROUTER_URL:-}" ]; then
  echo -e "\n${YELLOW}Please enter your Gemini Smart Router URL (e.g. https://my-router-xxx.run.app):${NC}"
  read -r INPUT_URL
  ROUTER_URL="$INPUT_URL"
fi

if [ -z "${ROUTER_API_KEY:-}" ]; then
  echo -e "\n${YELLOW}Please enter your Gemini Smart Router API Key:${NC}"
  read -rs INPUT_KEY
  ROUTER_API_KEY="$INPUT_KEY"
  echo ""
fi

if [ -z "$ROUTER_URL" ] || [ -z "$ROUTER_API_KEY" ]; then
  echo -e "${RED}❌ Error: Both ROUTER_URL and ROUTER_API_KEY are required for deployment.${NC}"
  exit 1
fi

# 4. Build and deploy using Google Cloud Build and Cloud Run
echo -e "\n${BLUE}📦 Building and deploying container image...${NC}"
gcloud run deploy "$SERVICE_NAME" \
  --source . \
  --platform managed \
  --region "$REGION" \
  --no-allow-unauthenticated \
  --set-env-vars="ROUTER_URL=$ROUTER_URL,ROUTER_API_KEY=$ROUTER_API_KEY"

# 5. Retrieve service URL
CLIENT_URL=$(gcloud run services describe "$SERVICE_NAME" --platform managed --region "$REGION" --format="value(status.url)")

echo -e "\n${GREEN}=======================================================${NC}"
echo -e "${GREEN}🎉 Deployment Successful!${NC}"
echo -e "${GREEN}Client Service URL: ${YELLOW}$CLIENT_URL${NC}"
echo -e "${GREEN}=======================================================${NC}"
echo -e "\n${YELLOW}🔒 Security Note: This endpoint is authenticated (--no-allow-unauthenticated).${NC}"
echo -e "To call this endpoint, you must include a Google OIDC ID token with Run Invoker permissions."
echo -e "\n${BLUE}👉 Test your endpoint with this curl command:${NC}"
echo -e "${GREEN}curl -X POST \"$CLIENT_URL/generate\" \\"
echo -e "  -H \"Authorization: Bearer \$(gcloud auth print-identity-token)\" \\"
echo -e "  -H \"Content-Type: application/json\" \\"
echo -e "  -d '{\"prompt\": \"Explain quantum computing in one short sentence.\"}'${NC}\n"
