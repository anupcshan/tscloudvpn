.PHONY: help test test-unit test-integration test-e2e clean build

# Default target
help:
	@echo "Available targets:"
	@echo "  build         - Build the tscloudvpn binary"
	@echo "  test          - Run all tests"
	@echo "  test-unit     - Run unit tests only"
	@echo "  test-integration - Run integration tests (fake provider)"
	@echo "  test-e2e      - Run end-to-end tests with real cloud providers"
	@echo "  test-e2e-{do,aws,gcp,vultr,linode,hetzner,azure} - Test a single provider"
	@echo "  clean         - Clean build artifacts"
	@echo ""
	@echo "E2E Test Examples:"
	@echo "  make test-e2e                              - Test all providers"
	@echo "  make test-e2e-aws                           - Test AWS only"
	@echo "  make test-e2e E2E_TIMEOUT=15m               - Custom timeout"
	@echo "  E2E_AWS_REGION=us-east-2 make test-e2e-aws  - Override region"

# Build targets
build:
	go build -o tscloudvpn ./cmd/tscloudvpn

clean:
	rm -f tscloudvpn
	@echo "Build artifacts cleaned"

# Test targets
test: test-unit test-integration
	@echo "All tests completed"

test-unit:
	@echo "Running unit tests..."
	go test -v ./internal/...

test-integration:
	@echo "Running integration tests with fake provider..."
	go test -v ./internal/instances -run TestIntegration

# E2E test targets
test-e2e:
	@echo "Running end-to-end tests with real cloud providers..."
	@echo "WARNING: This will create real cloud resources and incur costs!"
	@echo "Press Ctrl+C in the next 5 seconds to cancel..."
	@sleep 5
	REAL_INTEGRATION_TEST=true go test -tags=e2e -v ./internal/app -run TestExitNode -timeout=$(E2E_TIMEOUT)

E2E_TIMEOUT ?= 30m

test-e2e-do:
	@echo "Testing DigitalOcean only..."
	REAL_INTEGRATION_TEST=true go test -tags=e2e -v ./internal/app -run TestExitNode/do -timeout=$(E2E_TIMEOUT)

test-e2e-aws:
	@echo "Testing AWS EC2 only..."
	REAL_INTEGRATION_TEST=true go test -tags=e2e -v ./internal/app -run TestExitNode/ec2 -timeout=$(E2E_TIMEOUT)

test-e2e-gcp:
	@echo "Testing Google Cloud Platform only..."
	REAL_INTEGRATION_TEST=true go test -tags=e2e -v ./internal/app -run TestExitNode/gcp -timeout=$(E2E_TIMEOUT)

test-e2e-vultr:
	@echo "Testing Vultr only..."
	REAL_INTEGRATION_TEST=true go test -tags=e2e -v ./internal/app -run TestExitNode/vultr -timeout=$(E2E_TIMEOUT)

test-e2e-linode:
	@echo "Testing Linode only..."
	REAL_INTEGRATION_TEST=true go test -tags=e2e -v ./internal/app -run TestExitNode/linode -timeout=$(E2E_TIMEOUT)

test-e2e-hetzner:
	@echo "Testing Hetzner only..."
	REAL_INTEGRATION_TEST=true go test -tags=e2e -v ./internal/app -run TestExitNode/hetzner -timeout=$(E2E_TIMEOUT)

test-e2e-azure:
	@echo "Testing Azure only..."
	REAL_INTEGRATION_TEST=true go test -tags=e2e -v ./internal/app -run TestExitNode/azure -timeout=$(E2E_TIMEOUT)

# Credential check targets
check-credentials:
	@echo "Checking for cloud provider credentials..."
	@$(if $(DIGITALOCEAN_TOKEN),echo "✓ DigitalOcean credentials found",echo "✗ DigitalOcean credentials missing (DIGITALOCEAN_TOKEN)")
	@$(if $(VULTR_API_KEY),echo "✓ Vultr credentials found",echo "✗ Vultr credentials missing (VULTR_API_KEY)")
	@$(if $(LINODE_TOKEN),echo "✓ Linode credentials found",echo "✗ Linode credentials missing (LINODE_TOKEN)")
	@$(if $(or $(AWS_ACCESS_KEY_ID),$(shell test -f ~/.aws/credentials && echo "found")),echo "✓ AWS credentials found",echo "✗ AWS credentials missing (AWS_ACCESS_KEY_ID or ~/.aws/credentials)")
	@$(if $(or $(GOOGLE_APPLICATION_CREDENTIALS),$(GCP_CREDENTIALS_JSON_FILE)),echo "✓ GCP credentials found",echo "✗ GCP credentials missing (GOOGLE_APPLICATION_CREDENTIALS)")
	@$(if $(HETZNER_TOKEN),echo "✓ Hetzner credentials found",echo "✗ Hetzner credentials missing (HETZNER_TOKEN)")
	@$(if $(AZURE_SUBSCRIPTION_ID),echo "✓ Azure credentials found",echo "✗ Azure credentials missing (AZURE_SUBSCRIPTION_ID)")
	@$(if $(SSH_PUBKEY),echo "✓ SSH public key found",echo "✗ SSH public key missing (SSH_PUBKEY)")
	@echo ""
	@echo "To set up credentials, see E2E_TESTING.md"

# CI targets
ci-test: test

ci-test-e2e:
	@echo "Running E2E tests in CI mode..."
	# Set shorter timeout and skip interactive warnings in CI
	REAL_INTEGRATION_TEST=true go test -tags=e2e -v ./internal/app -run TestExitNode -timeout=20m
