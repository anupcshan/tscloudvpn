<img src="./cmd/tscloudvpn/assets/logo.svg" width="100" height="100" />

---

# tscloudvpn

[![Go Reference](https://pkg.go.dev/badge/github.com/tscloudvpn/tscloudvpn.svg)](https://pkg.go.dev/github.com/tscloudvpn/tscloudvpn)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](https://github.com/tscloudvpn/tscloudvpn/blob/main/LICENSE)

tscloudvpn is a tool for automatically managing VPN instances across multiple cloud providers with Tailscale/Headscale integration. It provides a web interface for easy management of cloud VPN exit nodes.

## Features

- Support for multiple cloud providers:
  - DigitalOcean
  - AWS EC2
  - Google Cloud Platform (GCP)
  - Hetzner Cloud
  - Linode
  - Vultr
- Integration with both Tailscale and Headscale control APIs
- Automated instance creation and management
- Web-based management interface
- Real-time instance status monitoring
- SSH key support for instance access

## Prerequisites

- Go 1.23 or later
- SSH public key for instance access
- API credentials for your chosen cloud provider(s)
- Tailscale account with OAuth client ID and secret, or Headscale API key and URL

## Installation

```bash
go install github.com/anupcshan/tscloudvpn/cmd/tscloudvpn@latest
```

## Configuration

tscloudvpn supports both YAML configuration files and environment variables. The configuration file is searched for in the following locations:

1. $XDG_CONFIG_HOME/tscloudvpn/config.yaml
2. ~/.config/tscloudvpn/config.yaml
3. ~/.tscloudvpn.yaml

### Configuration File (Recommended)

Example config.yaml:
```yaml
ssh:
  public_key: "ssh-rsa AAAA..."

control:
  type: "tailscale"  # or "headscale"
  tailscale:
    client_id: "..."
    client_secret: "..."
    tailnet: "..."
  headscale:
    api: "..."
    url: "..."
    api_key: "..."
    user: "..."

providers:
  digitalocean:
    token: "..."
  gcp:
    credentials_json: "..."
    project_id: "..."
    service_account: "..."
  hetzner:
    token: "..."
  vultr:
    api_key: "..."
  linode:
    token: "..."
  aws:
    # Either specify the credentials directly
    access_key: "..."
    secret_key: "..."
    session_token: "..."
    # ... or use the shared config dir
    shared_config_dir: "~/.aws"  # optional
    # ... or use the AWS_ environment variables
```

### Environment Variables (Legacy Support)

The following environment variables are still supported for backward compatibility:

#### Common Configuration
- `SSH_PUBKEY`: Your SSH public key for instance access

#### Tailscale Configuration
- `TAILSCALE_CLIENT_ID`: OAuth client ID
- `TAILSCALE_CLIENT_SECRET`: OAuth client secret
- `TAILSCALE_TAILNET`: Your tailnet name

#### Headscale Configuration (Alternative to Tailscale)
- `HEADSCALE_API`: Headscale API endpoint
- `HEADSCALE_URL`: Headscale URL
- `HEADSCALE_APIKEY`: Headscale API key
- `HEADSCALE_USER`: Headscale username

#### Cloud Provider Configuration

Configure your chosen cloud provider(s) by setting their respective environment variables:

- DigitalOcean: `DIGITALOCEAN_TOKEN`
- GCP:
  - `GCP_CREDENTIALS_JSON_FILE`
  - `GCP_PROJECT_ID`
  - `GCP_SERVICE_ACCOUNT`
- Hetzner Cloud: `HETZNER_TOKEN`
- Vultr: `VULTR_API_KEY`
- Linode: `LINODE_TOKEN`
- AWS: Uses standard AWS environment variables and ~/.aws/credentials

## Usage

1. Create a configuration file or set up the required environment variables
2. Run the tscloudvpn server:
   ```bash
   tscloudvpn
   ```
3. Access the web interface through your Tailscale/Headscale network on port 80
4. Use the interface to:
   - View available regions across providers
   - Launch new VPN instances
   - Monitor instance status
   - Manage exit nodes

## How It Works

1. When launching a new instance:
   - Creates an auth key for the new instance
   - Launches instance in the selected cloud provider
   - Waits for instance to become available (up to 1 minute)
   - Monitors instance registration with Tailscale/Headscale
   - Automatically approves the instance as an exit node

2. The web interface provides:
   - Real-time status of instances
   - Provider and region selection
   - Instance management controls
   - Overview of active nodes

## Development

1. Clone the repository:
   ```bash
   git clone https://github.com/anupcshan/tscloudvpn.git
   ```

2. Install dependencies:
   ```bash
   go mod download
   ```

3. Build the project:
   ```bash
   go build ./cmd/tscloudvpn
   ```

## Testing

### Unit and Integration Tests

Run the standard test suite:

```bash
make test
```

Or run specific test types:

```bash
make test-unit        # Unit tests only
make test-integration # Integration tests with fake provider
```

### End-to-End Tests

**WARNING**: E2E tests create real cloud resources and incur costs!

The project includes comprehensive end-to-end integration tests that:
- Create actual cloud instances across all supported providers
- Test complete instance lifecycle management
- Verify cloud provider API integration
- Test resource cleanup and deletion
- Validate pricing and region information

See [E2E_TESTING.md](E2E_TESTING.md) for detailed setup and usage instructions.

Quick start:

```bash
# Check what providers you can test
./scripts/run-e2e-tests.sh -n

# Set credentials (example for DigitalOcean)
export DIGITALOCEAN_TOKEN="your-token"
export SSH_PUBKEY="$(cat ~/.ssh/id_rsa.pub)"

# Run E2E tests
./scripts/run-e2e-tests.sh

# Or use make targets
make test-e2e                    # All providers
make test-e2e PROVIDER=do        # DigitalOcean only
make test-e2e-parallel           # Parallel testing
```

## License

Copyright (c) 2023, Anup Chenthamarakshan

Licensed under the BSD 3-Clause License. See [LICENSE](LICENSE) for the full text.
