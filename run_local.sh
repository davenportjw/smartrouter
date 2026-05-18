#!/bin/bash
# Gemini Smart Router local development startup script
# Highly polished for instant boot, mock configurations, and real Vertex AI calls.

set -e

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
BLUE='\033[0;34m'
NC='\033[0;30m' # No Color

log_info() {
    echo -e "${BLUE}[INFO] $1${NC}"
}

log_success() {
    echo -e "${GREEN}[SUCCESS] $1${NC}"
}

log_warn() {
    echo -e "${YELLOW}[WARN] $1${NC}"
}

log_error() {
    echo -e "${RED}[ERROR] $1${NC}"
}

# 1. Verify and Load Env variables
if [ ! -f .env ]; then
    log_warn ".env file not found in root directory. Copying from .env.sample..."
    if [ -f .env.sample ]; then
        cp .env.sample .env
    else
        log_error "No .env or .env.sample found. Please create one with GOOGLE_CLOUD_PROJECT"
        exit 1
    fi
fi

log_info "Loading environment configurations from .env..."
# Export variables from .env, filtering out comments
export $(grep -v '^#' .env | xargs)

# Validate standard GCP baseline
if [ -z "$GOOGLE_CLOUD_PROJECT" ]; then
    log_warn "GOOGLE_CLOUD_PROJECT variable not found. Upstream Vertex AI proxy calls may fail unless specified."
fi

# 2. Force Local Dev configurations
export LOCAL_DEV="true"
export PORT="8080"

# 3. Rebuild dynamic HTML components
log_info "Compiling Go HTML Templ components..."
go run github.com/a-h/templ/cmd/templ generate

# 4. Gorgeous header and boot information
echo -e "\n----------------------------------------------------------------------"
echo -e "     ${GREEN}G E M I N I   S M A R T   R O U T E R   ( L O C A L   D E V )${NC}"
echo -e "----------------------------------------------------------------------"
echo -e "👉 Local Server: ${GREEN}http://localhost:8080${NC}"
echo -e "👉 Admin Portal: ${GREEN}http://localhost:8080/login${NC}"
echo -e "👉 Mode        : ${YELLOW}Local Development (Mock DB & Mock Firebase Auth)${NC}"
echo -e "👉 Upstream API: ${BLUE}LIVE Vertex AI Gemini (via your local credentials)${NC}"
echo -e "----------------------------------------------------------------------"
log_info "Starting Go server...\n"

# 5. Launch server
go run main.go
