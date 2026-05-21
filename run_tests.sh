#!/bin/bash
# Smart Router local test and coverage runner script

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

# Clean up any leftover coverage profiles
cleanup() {
    rm -f coverage.out
}
trap cleanup EXIT

# 1. Run dynamic UI code compilation to ensure latest templates compile
log_info "Compiling latest Go HTML Templ components..."
go run github.com/a-h/templ/cmd/templ generate

# 2. Run tests with transitive coverage tracking
log_info "Executing test suite with transitive statement coverage profiling..."
go test -coverpkg=./... -coverprofile=coverage.out ./...

# 3. Print summary
echo -e "\n----------------------------------------------------------------------"
echo -e "     ${GREEN}S M A R T   R O U T E R   -   T E S T   C O V E R A G E${NC}"
echo -e "----------------------------------------------------------------------"

# Print function level details for newly added or modified packages
echo -e "\n👉 ${BLUE}Transitive Coverage Summary by Function:${NC}"
go tool cover -func=coverage.out | grep -E "pkg/config|limiter|auth|store.go" || true

# Print total overall statement coverage
echo -e "\n----------------------------------------------------------------------"
TOTAL_COV=$(go tool cover -func=coverage.out | grep "total:" | awk '{print $3}')
echo -e "🏆 ${GREEN}Total Transitive Statement Coverage: $TOTAL_COV${NC}"
echo -e "----------------------------------------------------------------------\n"

log_success "All tests passed successfully!"
