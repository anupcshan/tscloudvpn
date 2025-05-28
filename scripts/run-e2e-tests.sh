#!/bin/bash
set -euo pipefail

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Default values
TEST_TYPE="lifecycle"
PROVIDER=""
TIMEOUT="10m"
SKIP_CLEANUP="false"
VERBOSE="false"
DRY_RUN="false"

# Usage function
usage() {
    cat << EOF
Usage: $0 [OPTIONS]

Run end-to-end integration tests for cloud providers.

OPTIONS:
    -h, --help              Show this help message
    -t, --type TYPE         Test type: lifecycle, parallel, stress (default: lifecycle)
    -p, --provider PROVIDER Test specific provider: do, vultr, linode, ec2, gcp
    -T, --timeout DURATION  Test timeout (default: 10m)
    -s, --skip-cleanup      Skip resource cleanup (for debugging)
    -v, --verbose           Verbose output
    -n, --dry-run           Check credentials and show what would run

EXAMPLES:
    $0                                  # Test all providers with lifecycle test
    $0 -p do                           # Test only DigitalOcean
    $0 -t parallel                     # Run parallel test on all providers
    $0 -t stress -p vultr             # Run stress test on Vultr only
    $0 -n                              # Check credentials without running tests
    $0 -s -p do                        # Test DigitalOcean but skip cleanup

ENVIRONMENT VARIABLES:
    Required for each provider you want to test:
    - DIGITALOCEAN_TOKEN
    - VULTR_API_KEY
    - LINODE_TOKEN
    - AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY
    - GOOGLE_APPLICATION_CREDENTIALS + GCP_PROJECT_ID
    - SSH_PUBKEY

EOF
}

# Parse command line arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        -h|--help)
            usage
            exit 0
            ;;
        -t|--type)
            TEST_TYPE="$2"
            shift 2
            ;;
        -p|--provider)
            PROVIDER="$2"
            shift 2
            ;;
        -T|--timeout)
            TIMEOUT="$2"
            shift 2
            ;;
        -s|--skip-cleanup)
            SKIP_CLEANUP="true"
            shift
            ;;
        -v|--verbose)
            VERBOSE="true"
            shift
            ;;
        -n|--dry-run)
            DRY_RUN="true"
            shift
            ;;
        *)
            echo -e "${RED}Unknown option: $1${NC}"
            usage
            exit 1
            ;;
    esac
done

# Validate test type
case $TEST_TYPE in
    lifecycle|parallel|stress)
        ;;
    *)
        echo -e "${RED}Invalid test type: $TEST_TYPE${NC}"
        echo "Valid types: lifecycle, parallel, stress"
        exit 1
        ;;
esac

# Validate provider if specified
if [[ -n "$PROVIDER" ]]; then
    case $PROVIDER in
        do|vultr|linode|ec2|gcp)
            ;;
        *)
            echo -e "${RED}Invalid provider: $PROVIDER${NC}"
            echo "Valid providers: do, vultr, linode, ec2, gcp"
            exit 1
            ;;
    esac
fi

# Function to check if a command exists
command_exists() {
    command -v "$1" >/dev/null 2>&1
}

# Check prerequisites
echo -e "${BLUE}Checking prerequisites...${NC}"

if ! command_exists go; then
    echo -e "${RED}❌ Go is not installed${NC}"
    exit 1
fi

GO_VERSION=$(go version | cut -d' ' -f3 | sed 's/go//')
echo -e "${GREEN}✓ Go version: $GO_VERSION${NC}"

# Check credentials
echo -e "\n${BLUE}Checking cloud provider credentials...${NC}"

ENABLED_PROVIDERS=()

# DigitalOcean
if [[ -n "${DIGITALOCEAN_TOKEN:-}" ]]; then
    echo -e "${GREEN}✓ DigitalOcean credentials found${NC}"
    ENABLED_PROVIDERS+=("do")
else
    echo -e "${YELLOW}✗ DigitalOcean credentials missing (DIGITALOCEAN_TOKEN)${NC}"
fi

# Vultr
if [[ -n "${VULTR_API_KEY:-}" ]]; then
    echo -e "${GREEN}✓ Vultr credentials found${NC}"
    ENABLED_PROVIDERS+=("vultr")
else
    echo -e "${YELLOW}✗ Vultr credentials missing (VULTR_API_KEY)${NC}"
fi

# Linode
if [[ -n "${LINODE_TOKEN:-}" ]]; then
    echo -e "${GREEN}✓ Linode credentials found${NC}"
    ENABLED_PROVIDERS+=("linode")
else
    echo -e "${YELLOW}✗ Linode credentials missing (LINODE_TOKEN)${NC}"
fi

# AWS
if [[ -n "${AWS_ACCESS_KEY_ID:-}" && -n "${AWS_SECRET_ACCESS_KEY:-}" ]] || [[ -f ~/.aws/credentials ]]; then
    echo -e "${GREEN}✓ AWS credentials found${NC}"
    ENABLED_PROVIDERS+=("ec2")
else
    echo -e "${YELLOW}✗ AWS credentials missing (AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY or ~/.aws/credentials)${NC}"
fi

# GCP
if [[ -n "${GOOGLE_APPLICATION_CREDENTIALS:-}" && -n "${GCP_PROJECT_ID:-}" ]]; then
    echo -e "${GREEN}✓ GCP credentials found${NC}"
    ENABLED_PROVIDERS+=("gcp")
else
    echo -e "${YELLOW}✗ GCP credentials missing (GOOGLE_APPLICATION_CREDENTIALS + GCP_PROJECT_ID)${NC}"
fi

# SSH Key
if [[ -n "${SSH_PUBKEY:-}" ]]; then
    echo -e "${GREEN}✓ SSH public key found${NC}"
elif [[ -f ~/.ssh/id_rsa.pub ]]; then
    export SSH_PUBKEY="$(cat ~/.ssh/id_rsa.pub)"
    echo -e "${GREEN}✓ SSH public key loaded from ~/.ssh/id_rsa.pub${NC}"
else
    echo -e "${YELLOW}✗ SSH public key missing (SSH_PUBKEY or ~/.ssh/id_rsa.pub)${NC}"
    echo -e "${YELLOW}Generating temporary SSH key...${NC}"
    ssh-keygen -t rsa -b 2048 -f ~/.ssh/id_rsa_temp -N "" >/dev/null 2>&1
    export SSH_PUBKEY="$(cat ~/.ssh/id_rsa_temp.pub)"
    echo -e "${GREEN}✓ Temporary SSH key generated${NC}"
fi

# Check if any providers are enabled
if [[ ${#ENABLED_PROVIDERS[@]} -eq 0 ]]; then
    echo -e "\n${RED}No cloud provider credentials found!${NC}"
    echo "Please set environment variables for at least one provider."
    echo "See E2E_TESTING.md for setup instructions."
    exit 1
fi

echo -e "\n${GREEN}Found credentials for: ${ENABLED_PROVIDERS[*]}${NC}"

# If specific provider requested, check if it's available
if [[ -n "$PROVIDER" ]]; then
    if [[ ! " ${ENABLED_PROVIDERS[*]} " =~ " ${PROVIDER} " ]]; then
        echo -e "${RED}Provider $PROVIDER requested but credentials not found${NC}"
        exit 1
    fi
    ENABLED_PROVIDERS=("$PROVIDER")
fi

# Determine test command
case $TEST_TYPE in
    lifecycle)
        if [[ -n "$PROVIDER" ]]; then
            TEST_NAME="TestE2E_AllProvidersLifecycle/Provider_$PROVIDER"
        else
            TEST_NAME="TestE2E_AllProvidersLifecycle"
        fi
        ;;
    parallel)
        TEST_NAME="TestE2E_MultipleProvidersParallel"
        ;;
    stress)
        TEST_NAME="TestE2E_StressTest"
        export E2E_STRESS_TEST=true
        ;;
esac

# Set environment variables
export E2E_TIMEOUT="$TIMEOUT"
if [[ "$SKIP_CLEANUP" == "true" ]]; then
    export E2E_SKIP_CLEANUP=true
fi

# Calculate estimated cost
NUM_PROVIDERS=${#ENABLED_PROVIDERS[@]}
EST_COST_PER_PROVIDER="0.002"
# Simple multiplication without bc dependency
if command -v bc >/dev/null 2>&1; then
    EST_TOTAL_COST=$(echo "$NUM_PROVIDERS * $EST_COST_PER_PROVIDER" | bc -l)
else
    EST_TOTAL_COST="~$(printf "%.3f" "$(awk "BEGIN {print $NUM_PROVIDERS * $EST_COST_PER_PROVIDER}" 2>/dev/null || echo "$NUM_PROVIDERS x $EST_COST_PER_PROVIDER")") "
fi

echo -e "\n${BLUE}Test Configuration:${NC}"
echo "  Test type: $TEST_TYPE"
echo "  Providers: ${ENABLED_PROVIDERS[*]}"
echo "  Timeout: $TIMEOUT"
echo "  Skip cleanup: $SKIP_CLEANUP"
echo "  Estimated cost: ~\$${EST_TOTAL_COST} USD"

if [[ "$DRY_RUN" == "true" ]]; then
    echo -e "\n${GREEN}Dry run complete. Tests would run with the above configuration.${NC}"
    exit 0
fi

# Warning about costs
echo -e "\n${YELLOW}⚠️  WARNING: These tests will create real cloud resources and incur costs!${NC}"
if [[ "$SKIP_CLEANUP" == "true" ]]; then
    echo -e "${RED}⚠️  Cleanup is disabled - resources will be left running!${NC}"
fi

# Ask for confirmation unless in CI
if [[ "${CI:-false}" != "true" && "${GITHUB_ACTIONS:-false}" != "true" ]]; then
    echo -n "Do you want to continue? [y/N] "
    read -r response
    if [[ ! "$response" =~ ^[Yy]$ ]]; then
        echo "Aborted."
        exit 0
    fi
fi

# Build test command
TEST_CMD="go test -tags=e2e -v ./internal/providers -run '$TEST_NAME' -timeout=30m"

if [[ "$VERBOSE" == "true" ]]; then
    TEST_CMD="$TEST_CMD 2>&1 | tee test.log"
fi

echo -e "\n${BLUE}Running E2E tests...${NC}"
echo "Command: $TEST_CMD"
echo ""

# Change to the correct directory
cd "$(dirname "$0")/.."

# Run the test
START_TIME=$(date +%s)
set +e
eval "$TEST_CMD"
TEST_EXIT_CODE=$?
set -e
END_TIME=$(date +%s)
ELAPSED=$((END_TIME - START_TIME))

echo -e "\n${BLUE}Test Summary:${NC}"
echo "  Duration: ${ELAPSED}s"
echo "  Exit code: $TEST_EXIT_CODE"

if [[ $TEST_EXIT_CODE -eq 0 ]]; then
    echo -e "  Status: ${GREEN}✓ PASSED${NC}"
else
    echo -e "  Status: ${RED}❌ FAILED${NC}"
fi

if [[ "$SKIP_CLEANUP" == "true" ]]; then
    echo -e "\n${YELLOW}⚠️  Remember to manually clean up any remaining resources!${NC}"
    echo "Check your cloud provider consoles for instances with 'tscloudvpn' in the name."
fi

echo -e "\n${BLUE}Logs and artifacts:${NC}"
if [[ -f "test.log" ]]; then
    echo "  Test log: test.log"
fi
echo "  For detailed setup instructions, see: E2E_TESTING.md"

exit $TEST_EXIT_CODE
