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

The following environment variables are required depending on your setup:

### Common Configuration
- `SSH_PUBKEY`: Your SSH public key for instance access

### Tailscale Configuration
- `TAILSCALE_CLIENT_ID`: OAuth client ID
- `TAILSCALE_CLIENT_SECRET`: OAuth client secret
- `TAILSCALE_TAILNET`: Your tailnet name

### Headscale Configuration (Alternative to Tailscale)
- `HEADSCALE_API`: Headscale API endpoint
- `HEADSCALE_URL`: Headscale URL
- `HEADSCALE_APIKEY`: Headscale API key
- `HEADSCALE_USER`: Headscale username

### Cloud Provider Configuration

Configure your chosen cloud provider(s) by setting their respective environment variables. Refer to each provider's documentation for specific API credential requirements:

- DigitalOcean: Digital Ocean API token
- AWS EC2: AWS credentials and region
- GCP: Google Cloud credentials
- Linode: Linode API token
- Vultr: Vultr API key

## Usage

1. Set up the required environment variables for your chosen configuration
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

## License

Copyright (c) 2023, Anup Chenthamarakshan

Licensed under the BSD 3-Clause License. See [LICENSE](LICENSE) for the full text.
