.PHONY: help test test-unit test-integration test-e2e clean build

# Default target
help:
	@echo "Available targets:"
	@echo "  build         - Build the tscloudvpn binary"
	@echo "  test          - Run all tests"
	@echo "  test-unit     - Run unit tests only"
	@echo "  test-integration - Run integration tests (fake provider)"
	@echo "  test-e2e      - Run end-to-end tests with real cloud providers"
	@echo "  test-e2e-quick - Run E2E tests with shorter timeout"
	@echo "  test-e2e-stress - Run E2E stress tests"
	@echo "  test-e2e-parallel - Run E2E tests in parallel"
	@echo "  clean         - Clean build artifacts"
	@echo ""
	@echo "E2E Test Examples:"
	@echo "  make test-e2e                    - Test all configured providers"
	@echo "  make test-e2e PROVIDER=do        - Test only DigitalOcean"
	@echo "  make test-e2e REGION_DO=fra1     - Use Frankfurt region for DO"
	@echo "  make test-e2e TIMEOUT=5m         - Use 5 minute timeout"
	@echo ""
	@echo "Environment variables for E2E tests:"
	@echo "  DIGITALOCEAN_TOKEN, VULTR_API_KEY, LINODE_TOKEN"
	@echo "  AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY"
	@echo "  GOOGLE_APPLICATION_CREDENTIALS, GCP_PROJECT_ID"
	@echo "  SSH_PUBKEY"

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
	$(call run-e2e-test,TestE2E_AllProvidersLifecycle)

test-e2e-quick:
	@echo "Running quick E2E tests (5 minute timeout)..."
	E2E_TIMEOUT=5m $(call run-e2e-test,TestE2E_AllProvidersLifecycle)

test-e2e-parallel:
	@echo "Running E2E tests in parallel..."
	$(call run-e2e-test,TestE2E_MultipleProvidersParallel)

test-e2e-stress:
	@echo "Running E2E stress tests..."
	E2E_STRESS_TEST=true $(call run-e2e-test,TestE2E_StressTest)

# Helper function to run E2E tests
define run-e2e-test
	$(eval TEST_ARGS := -tags=e2e -v ./internal/providers -timeout=30m)
	$(if $(PROVIDER),$(eval TEST_ARGS += -run $(1)/Provider_$(PROVIDER)),$(eval TEST_ARGS += -run $(1)))
	$(if $(TIMEOUT),$(eval TEST_ARGS := $(subst -timeout=30m,-timeout=$(TIMEOUT),$(TEST_ARGS))))
	$(if $(REGION_DO),export E2E_DO_REGION=$(REGION_DO);)
	$(if $(REGION_AWS),export E2E_AWS_REGION=$(REGION_AWS);)
	$(if $(REGION_GCP),export E2E_GCP_REGION=$(REGION_GCP);)
	$(if $(REGION_VULTR),export E2E_VULTR_REGION=$(REGION_VULTR);)
	$(if $(REGION_LINODE),export E2E_LINODE_REGION=$(REGION_LINODE);)
	go test $(TEST_ARGS)
endef

# Development targets
test-e2e-no-cleanup:
	@echo "Running E2E tests without cleanup (for debugging)..."
	@echo "WARNING: Instances will be left running - remember to clean up manually!"
	E2E_SKIP_CLEANUP=true $(call run-e2e-test,TestE2E_AllProvidersLifecycle)

test-e2e-do:
	@echo "Testing DigitalOcean only..."
	$(call run-e2e-test,TestE2E_AllProvidersLifecycle/Provider_do)

test-e2e-aws:
	@echo "Testing AWS EC2 only..."
	$(call run-e2e-test,TestE2E_AllProvidersLifecycle/Provider_ec2)

test-e2e-gcp:
	@echo "Testing Google Cloud Platform only..."
	$(call run-e2e-test,TestE2E_AllProvidersLifecycle/Provider_gcp)

test-e2e-vultr:
	@echo "Testing Vultr only..."
	$(call run-e2e-test,TestE2E_AllProvidersLifecycle/Provider_vultr)

test-e2e-linode:
	@echo "Testing Linode only..."
	$(call run-e2e-test,TestE2E_AllProvidersLifecycle/Provider_linode)

# Credential check targets
check-credentials:
	@echo "Checking for cloud provider credentials..."
	@$(if $(DIGITALOCEAN_TOKEN),echo "✓ DigitalOcean credentials found",echo "✗ DigitalOcean credentials missing (DIGITALOCEAN_TOKEN)")
	@$(if $(VULTR_API_KEY),echo "✓ Vultr credentials found",echo "✗ Vultr credentials missing (VULTR_API_KEY)")
	@$(if $(LINODE_TOKEN),echo "✓ Linode credentials found",echo "✗ Linode credentials missing (LINODE_TOKEN)")
	@$(if $(or $(AWS_ACCESS_KEY_ID),$(shell test -f ~/.aws/credentials && echo "found")),echo "✓ AWS credentials found",echo "✗ AWS credentials missing (AWS_ACCESS_KEY_ID or ~/.aws/credentials)")
	@$(if $(or $(GOOGLE_APPLICATION_CREDENTIALS),$(GCP_CREDENTIALS_JSON_FILE)),echo "✓ GCP credentials found",echo "✗ GCP credentials missing (GOOGLE_APPLICATION_CREDENTIALS)")
	@$(if $(SSH_PUBKEY),echo "✓ SSH public key found",echo "✗ SSH public key missing (SSH_PUBKEY)")
	@echo ""
	@echo "To set up credentials, see E2E_TESTING.md"

# CI targets
ci-test: test

ci-test-e2e:
	@echo "Running E2E tests in CI mode..."
	# Set shorter timeout and skip interactive warnings in CI
	E2E_TIMEOUT=15m go test -tags=e2e -v ./internal/providers -timeout=20m
