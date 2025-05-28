# End-to-End Integration Testing

This document describes how to run comprehensive end-to-end integration tests for all cloud providers supported by tscloudvpn.

## Overview

The end-to-end tests create actual cloud instances, verify VPN connectivity, test internet access through exit nodes, and clean up resources. These tests incur real costs and require valid cloud provider credentials.

## Test Coverage

The E2E tests cover:

1. **Instance Lifecycle**: Create, verify running status, delete instances
2. **Network Connectivity**: Test direct (non-relayed) VPN connections
3. **Internet Access**: Verify internet connectivity through exit nodes
4. **Cloud Provider APIs**: List regions, get pricing, manage instances
5. **Control Plane Integration**: Device registration, exit node approval
6. **Resource Cleanup**: Ensure all created resources are properly deleted

## Supported Providers

- **AWS EC2** (`ec2`)
- **Google Cloud Platform** (`gcp`)
- **DigitalOcean** (`do`)
- **Vultr** (`vultr`)
- **Linode** (`linode`)

## Prerequisites

### 1. Install Dependencies

```bash
go mod download
```

### 2. Set Up Cloud Provider Credentials

#### AWS EC2
```bash
export AWS_ACCESS_KEY_ID="your-access-key"
export AWS_SECRET_ACCESS_KEY="your-secret-key"
export AWS_DEFAULT_REGION="us-east-1"  # Optional
# OR configure ~/.aws/credentials
```

#### Google Cloud Platform
```bash
export GOOGLE_APPLICATION_CREDENTIALS="/path/to/service-account.json"
export GCP_PROJECT_ID="your-project-id"
export GCP_SERVICE_ACCOUNT="your-service-account@project.iam.gserviceaccount.com"
```

#### DigitalOcean
```bash
export DIGITALOCEAN_TOKEN="your-api-token"
```

#### Vultr
```bash
export VULTR_API_KEY="your-api-key"
```

#### Linode
```bash
export LINODE_TOKEN="your-api-token"
```

### 3. Set Up SSH Key
```bash
export SSH_PUBKEY="$(cat ~/.ssh/id_rsa.pub)"
```

### 4. Configure VPN Control Plane (Optional)

For Tailscale:
```bash
export TAILSCALE_CLIENT_ID="your-client-id"
export TAILSCALE_CLIENT_SECRET="your-client-secret"
export TAILSCALE_TAILNET="your-tailnet"
```

For Headscale:
```bash
export HEADSCALE_API="https://your-headscale-server/api/v1"
export HEADSCALE_URL="https://your-headscale-server"
export HEADSCALE_APIKEY="your-api-key"
export HEADSCALE_USER="your-user"
```

## Running Tests

### Basic E2E Test

Run tests for all configured providers:

```bash
go test -tags=e2e -v ./internal/providers -run TestE2E_AllProvidersLifecycle
```

### Single Provider Test

Test only a specific provider:

```bash
# Test only DigitalOcean
DIGITALOCEAN_TOKEN="your-token" go test -tags=e2e -v ./internal/providers -run TestE2E_AllProvidersLifecycle/Provider_do
```

### Parallel Provider Testing

Test multiple providers simultaneously:

```bash
go test -tags=e2e -v ./internal/providers -run TestE2E_MultipleProvidersParallel
```

### Stress Testing

Run repeated create/delete cycles:

```bash
E2E_STRESS_TEST=true E2E_STRESS_ITERATIONS=5 go test -tags=e2e -v ./internal/providers -run TestE2E_StressTest
```

## Configuration Options

### Environment Variables

| Variable | Description | Default |
|----------|-------------|--------|
| `E2E_TIMEOUT` | Test timeout duration | `10m` |
| `E2E_SKIP_CLEANUP` | Skip resource cleanup (for debugging) | `false` |
| `E2E_STRESS_TEST` | Enable stress testing | `false` |
| `E2E_STRESS_ITERATIONS` | Number of stress test iterations | `3` |

### Region Override

Override default test regions:

```bash
export E2E_DO_REGION="fra1"          # DigitalOcean Frankfurt
export E2E_VULTR_REGION="lhr"        # Vultr London
export E2E_LINODE_REGION="eu-west"   # Linode London
export E2E_AWS_REGION="eu-west-1"    # AWS Ireland
export E2E_GCP_REGION="europe-west1-b" # GCP Belgium
```

## Default Test Regions

The tests use cost-optimized regions by default:

- **AWS**: `us-east-1` (Virginia)
- **GCP**: `us-central1-a` (Iowa)
- **DigitalOcean**: `nyc1` (New York)
- **Vultr**: `ewr` (Newark)
- **Linode**: `us-east` (Fremont)

## Test Scenarios

### 1. Complete Lifecycle Test

```bash
go test -tags=e2e -v ./internal/providers -run TestE2E_AllProvidersLifecycle
```

For each provider:
1. Lists available regions
2. Checks initial instance status (should be missing)
3. Creates a new instance
4. Waits for instance to be running
5. Verifies instance appears in provider's instance list
6. Tests network connectivity
7. Simulates VPN connection establishment
8. Tests direct connection verification
9. Deletes the instance
10. Verifies instance is completely removed

### 2. Parallel Multi-Provider Test

```bash
go test -tags=e2e -v ./internal/providers -run TestE2E_MultipleProvidersParallel
```

Runs lifecycle tests for all providers simultaneously to test:
- Concurrent instance creation
- Resource isolation between providers
- Parallel API handling

### 3. Stress Test

```bash
E2E_STRESS_TEST=true go test -tags=e2e -v ./internal/providers -run TestE2E_StressTest
```

Repeats create/delete cycles to test:
- Resource cleanup reliability
- API rate limiting handling
- Provider stability under repeated operations

## Cost Considerations

**WARNING**: These tests create real cloud resources and will incur costs.

### Estimated Costs Per Test Run

| Provider | Instance Type | Typical Cost/Hour | Per Test (~5-10 min) |
|----------|---------------|-------------------|----------------------|
| AWS EC2 | t3.micro | $0.0104 | ~$0.002 |
| GCP | e2-micro | $0.008 | ~$0.001 |
| DigitalOcean | s-1vcpu-1gb | $0.007 | ~$0.001 |
| Vultr | vc2-1c-1gb | $0.006 | ~$0.001 |
| Linode | nanode-1 | $0.0075 | ~$0.001 |

**Total per full test run**: ~$0.006 (less than 1 cent)

### Cost Optimization

1. **Use cheapest regions**: Tests default to cost-optimized regions
2. **Quick cleanup**: Instances are deleted immediately after testing
3. **Parallel testing**: Multiple providers tested simultaneously
4. **Short-lived**: Instances typically exist for 5-10 minutes

## Debugging Failed Tests

### Skip Cleanup for Investigation

```bash
E2E_SKIP_CLEANUP=true go test -tags=e2e -v ./internal/providers -run TestE2E_AllProvidersLifecycle
```

This leaves instances running for manual investigation. **Remember to manually delete them to avoid ongoing charges.**

### Verbose Logging

```bash
go test -tags=e2e -v ./internal/providers -run TestE2E_AllProvidersLifecycle 2>&1 | tee test.log
```

### Test Specific Provider

```bash
go test -tags=e2e -v ./internal/providers -run TestE2E_AllProvidersLifecycle/Provider_do
```

## CI/CD Integration

### GitHub Actions Example

```yaml
name: E2E Tests
on:
  schedule:
    - cron: '0 2 * * *'  # Daily at 2 AM
  workflow_dispatch:

jobs:
  e2e:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v4
        with:
          go-version: '1.24'
      
      - name: Run E2E Tests
        env:
          AWS_ACCESS_KEY_ID: ${{ secrets.AWS_ACCESS_KEY_ID }}
          AWS_SECRET_ACCESS_KEY: ${{ secrets.AWS_SECRET_ACCESS_KEY }}
          DIGITALOCEAN_TOKEN: ${{ secrets.DIGITALOCEAN_TOKEN }}
          VULTR_API_KEY: ${{ secrets.VULTR_API_KEY }}
          LINODE_TOKEN: ${{ secrets.LINODE_TOKEN }}
          GOOGLE_APPLICATION_CREDENTIALS_JSON: ${{ secrets.GCP_CREDENTIALS }}
          GCP_PROJECT_ID: ${{ secrets.GCP_PROJECT_ID }}
          SSH_PUBKEY: ${{ secrets.SSH_PUBKEY }}
        run: |
          echo "$GOOGLE_APPLICATION_CREDENTIALS_JSON" > gcp-creds.json
          export GOOGLE_APPLICATION_CREDENTIALS=gcp-creds.json
          go test -tags=e2e -v ./internal/providers -timeout=30m
```

## Security Considerations

1. **Credential Security**: Never commit credentials to version control
2. **Minimal Permissions**: Use IAM roles with minimal required permissions
3. **Temporary Credentials**: Use short-lived credentials when possible
4. **Network Security**: Tests create instances with minimal network exposure
5. **Resource Cleanup**: Always clean up resources to avoid security risks

## Troubleshooting

### Common Issues

#### "No cloud provider credentials found"
- Ensure environment variables are set correctly
- Check that credentials have sufficient permissions
- Verify credential format (especially for GCP JSON)

#### "Test region not found in available regions"
- Check if the specified region exists for the provider
- Verify region naming conventions (e.g., `us-east-1` vs `us-east`)
- Use `E2E_*_REGION` environment variables to override

#### "Failed to create instance"
- Check API quotas and limits
- Verify billing is enabled for the cloud account
- Ensure sufficient permissions for instance creation
- Check if region supports the instance type

#### "Instance not running after waiting"
- Some providers take longer to start instances
- Check cloud provider console for error messages
- Verify image availability in the region

### Manual Cleanup

If tests fail and leave resources running:

```bash
# AWS
aws ec2 describe-instances --filters "Name=tag:Name,Values=tscloudvpn-*"
aws ec2 terminate-instances --instance-ids i-1234567890abcdef0

# DigitalOcean
doctl compute droplet list --tag-name tscloudvpn
doctl compute droplet delete <droplet-id>

# And so on for other providers...
```

## Contributing

When adding new providers or modifying tests:

1. Follow the existing test structure in `testProviderLifecycle`
2. Add provider-specific environment variable documentation
3. Update cost estimates
4. Test with real credentials before submitting
5. Ensure proper cleanup in all code paths
