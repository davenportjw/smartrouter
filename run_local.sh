#!/bin/bash
# Smart Router local development startup script
# Decoupled Multi-Service orchestrator (Backend + Frontend)

set -e

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

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
export $(grep -v '^#' .env | xargs)

if [ -z "$GOOGLE_CLOUD_PROJECT" ]; then
    log_warn "GOOGLE_CLOUD_PROJECT variable not found. Upstream Vertex AI proxy calls may fail unless specified."
fi

# 2. Dynamic UI Code Compilation
log_info "Compiling Go HTML Templ components..."
go run github.com/a-h/templ/cmd/templ generate

# 3. Re-orchestration Parameters
export LOCAL_DEV="true"
export BACKEND_SHARED_SECRET="local-dev-bypass-token-12345"

# 4. Graceful exit traps
cleanup() {
    log_warn "Interrupt signal caught! Cleaning up running services..."
    if [ -n "$BACKEND_PID" ]; then
        kill "$BACKEND_PID" 2>/dev/null || true
    fi
    if [ -n "$FRONTEND_PID" ]; then
        kill "$FRONTEND_PID" 2>/dev/null || true
    fi
    log_success "Cleanup complete. Bye!"
    exit 0
}
trap cleanup SIGINT SIGTERM EXIT

# 5. Boot Up Services in Background
log_info "Starting Backend Service on port 8080..."
PORT="8080" go run backend/main.go &
BACKEND_PID=$!

# Wait 1.5 seconds to let backend boot and start listeners
sleep 1.5

log_info "Starting Frontend Service on port 8081..."
PORT="8081" BACKEND_API_URL="http://localhost:8080" go run frontend/main.go &
FRONTEND_PID=$!

# Gorgeous boot details
echo -e "\n----------------------------------------------------------------------"
echo -e "     ${GREEN}S M A R T   R O U T E R   ( D E C O U P L E D   L O C A L )${NC}"
echo -e "----------------------------------------------------------------------"
echo -e "👉 Backend URL  : ${GREEN}http://localhost:8080${NC}"
echo -e "👉 Admin Portal : ${GREEN}http://localhost:8081/login${NC}"
echo -e "👉 Mode         : ${YELLOW}Decoupled Services (Backend REST APIs + Frontend UI)${NC}"
echo -e "👉 Shared Secret: ${BLUE}local-dev-bypass-token-12345${NC}"
echo -e "----------------------------------------------------------------------\n"

# Wait on background tasks to keep process alive
wait
