# Dear LLM

This file contains important context and instructions for AI assistants working on this codebase.

## Project Overview

This is a Go-based cloud VPN project that creates and manages Tailscale exit nodes across multiple cloud providers. The application uses TSNet for Tailscale integration and provides a web interface for managing cloud instances.

## Key Information

- **Main branch**: `main`
- **Language**: Go
- **Frontend**: HTMX + Server-Side Events (SSE) + Bootstrap
- **Cloud providers supported**: AWS, Azure, GCP, DigitalOcean, Hetzner, Linode, Vultr
- **VPN Integration**: Tailscale (via TSNet) and Headscale support

## Architecture Principles

### Frontend Architecture
- **HTMX 1.9.12** for dynamic HTML updates without complex JavaScript
- **Server-Side Events (SSE)** for real-time updates via `/events` endpoint
- **Go HTML templates** with embedded assets
- **Bootstrap 3.3.7** for responsive UI styling
- **jQuery integration** for client-side filtering only
- **No custom frontend code** - only 3rd party libraries

### Backend Patterns
- **Provider Registry Pattern** - Each cloud provider registers via `init()` functions
- **Instance Controller Pattern** - One controller per cloud instance with background monitoring
- **Control API Abstraction** - Common interface for Tailscale and Headscale
- **Lazy Loading Pattern** - Cached region/pricing data with 24-hour expiration
- **Event Loop Pattern** - Generic event controller with action queues

### Cloud Provider Integration
- **7 supported providers**: AWS EC2, Azure, GCP, DigitalOcean, Hetzner, Linode, Vultr
- **Common Provider interface** with methods for instance lifecycle, regions, pricing
- **Provider-specific optimizations** for instance types and regional availability
- **Real-time pricing** integration with provider APIs

## Development Guidelines

### Code Patterns
- Follow existing Go idioms and patterns found in the codebase
- Use the common `Provider` interface for cloud provider implementations
- Implement proper error handling with context cancellation
- Use lazy loading with `utils.LazyWithErrors` for expensive operations
- Follow the event-driven controller pattern for instance management

### Configuration
- **YAML configuration** preferred over environment variables
- **XDG Base Directory** specification compliance
- **Provider-specific** configuration sections
- Check existing configuration patterns in the codebase

### Frontend Development
- Use **HTMX attributes** for dynamic content updates
- Implement **SSE endpoints** for real-time data streaming
- Follow the **three-template structure**: header.tmpl, content.tmpl, footer.tmpl
- Use **Bootstrap classes** for consistent styling
- Avoid custom JavaScript - leverage HTMX capabilities

### Testing & Quality
- Test changes across all supported cloud providers
- Ensure SSE events work correctly for real-time updates
- Verify HTMX interactions don't break existing functionality
- Test instance lifecycle management thoroughly

## Important Technical Details

- **TSNet Integration**: Application runs on Tailscale network with ephemeral auth keys
- **Health Monitoring**: Ring buffer for ping history, real-time statistics tracking
- **Security**: SSH key management, Tailscale network access, tag-based access control
- **Instance Lifecycle**: Automated setup scripts, self-monitoring, graceful shutdown
- **Resilience**: Timeout handling, retry logic, graceful degradation

## Common Operations

- Instance creation/deletion across providers
- Real-time status monitoring via SSE
- Pricing calculations and comparisons
- Regional deployment configurations
- Tailscale exit node management
- Health check monitoring and reporting