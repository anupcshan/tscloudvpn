# Service Runner Plan

tscloudvpn evolves from a single-purpose exit node launcher into a general-purpose
service runner: ephemeral cloud VMs on your Tailscale network, with optional
persistent storage and reproducible environments.

This is a living document. Phases are roughly ordered by dependency but the
boundaries are flexible.

---

## Architecture overview

```
                  tscloudvpn (orchestrator, always-on)
                  ├── Service type registry
                  ├── Provider registry (7 cloud providers)
                  ├── Volume manager (R2 credential lifecycle)
                  └── Instance registry (controllers, GC, health)
                         │
                         ▼
              ┌─────────────────────┐
              │   Ephemeral VM      │
              │                     │
              │  tailscale ─────── tailnet
              │  cloudnbd ─────── R2 (persistent storage)
              │  service binary     │
              │  stats endpoint     │
              └─────────────────────┘
```

### Key design decisions

- **Providers stay VM-focused.** They don't know about services. They receive
  a `CreateRequest` with VM requirements, user-data, and hostname. Service
  logic lives in the orchestration layer above.

- **Hostname convention:** `{service}-{name?}-{provider}-{region}`.
  - Exit node: `exit-do-nyc1`
  - Named file server: `files-photos-gcp-us-central1`
  - Android emulator: `emu-myphone-ec2-us-west-2`
  - Multi-instance suffix when needed: `emu-myphone-ec2-us-west-2-1`

- **Persistence via cloudnbd + R2.** cloudnbd runs on the VM itself (not the
  tscloudvpn host) so tscloudvpn is not in the data path and is not a SPOF.

- **Credential-based fencing.** Each VM gets a scoped R2 API token. To fence
  an old writer, tscloudvpn deletes the token via Cloudflare API (instant,
  synchronous). No locks, no leases, no clock coordination.

- **Token discovery.** Active R2 tokens are discovered from the Cloudflare API
  on startup (matched by naming convention). No local state to lose.

- **Reproducible environments via Nix.** Service environments are built with
  `nix build`, exported as tarballs, and uploaded to R2. VMs download once
  to persistent storage. Versioned, checksummed, rollbackable.

---

## Phase 0: Service type abstraction + file server

**Goal:** Refactor the codebase to support multiple service types. Validate
with a simple file server that exercises cloudnbd persistence.

### 0a: ServiceType abstraction

Introduce the `ServiceType` struct and thread it through the system.

```go
type ServiceType struct {
    Name           string            // DNS-safe short name: "exit", "files", "emu"
    Label          string            // Human-readable: "Exit Node", "File Server"
    InitScript     string            // cloud-init template
    TailscaleFlags []string          // e.g., ["--advertise-exit-node"]
    Tags           []string          // Tailscale ACL tags
    VMRequirements VMRequirements
    IdleTimeout    time.Duration
    StatsPath      string            // HTTP path to poll for stats ("" = none)
    ParseStats     func([]byte) (lastActive time.Time, err error)
    Persistence    *PersistenceConfig // nil = ephemeral
    Env            map[string]string  // version URLs, checksums, etc.
}

type VMRequirements struct {
    MinVCPUs   int
    MinRAMGB   int
    MinDiskGB  int
    NestedVirt bool
}

type PersistenceConfig struct {
    MountPoint string // "/data"
    Size       string // "10G"
    FSType     string // "zfs" (always ZFS for persistent services)
}
```

What changes:

| Layer | Change |
|-------|--------|
| `Provider` interface | `CreateInstance` takes `CreateRequest` (hostname, region, user-data, requirements). Init script templating moves out of providers into the orchestration layer. `Hostname()` and `GetRegionHourlyEstimate()` take service name. |
| Each provider impl | VM size selection respects `VMRequirements`. Init script no longer templated here. |
| `ControlApi.CreateKey` | Accepts `tags []string` parameter instead of hardcoding `tag:untrusted`. |
| `Controller` | Holds a `*ServiceType`. Uses its `IdleTimeout`, `StatsPath`, `ParseStats`. |
| `Registry` | Key becomes `{service}-{name}-{provider}-{region}`. `CreateInstance` takes service type + optional volume name. |
| `Creator` | Templates init script from `ServiceType.InitScript` before passing to provider. |
| `Manager` / UI / API | Routes become `/providers/{p}/services/{s}/regions/{r}` (or similar). UI shows service type. |
| `GC` | Updated hostname pattern, otherwise unchanged. |
| Config | Exit node defined as a `ServiceType` with current behavior, ensuring zero regression. |

The exit node is re-expressed as a ServiceType. All existing behavior is preserved.
Tests must pass with exit node working identically to today.

### 0b: VolumeManager + R2 credential lifecycle

New component: `VolumeManager`.

```go
type VolumeManager struct {
    cfAccountID string
    cfAPIToken  string        // master token (needs "API Tokens: Edit" + "R2: Edit")
    r2Bucket    string
    r2Endpoint  string
    mu          sync.Mutex
    volumes     map[string]*Volume
}

type Volume struct {
    Name        string
    Size        string
    S3Prefix    string          // "volumes/{owner}/{name}/"
    ActiveToken *VolumeToken    // nil if no VM using this volume
}

type VolumeToken struct {
    CloudflareTokenID string
    AccessKeyID       string
    SecretAccessKey    string
    VMHostname        string
}
```

- `Acquire(volumeName, vmHostname)` — revokes any existing token, creates new
  scoped R2 token, returns credentials for the VM's cloud-init.
- `Release(volumeName)` — revokes the active token. Called BEFORE VM deletion.
- On startup: list all Cloudflare API tokens matching naming convention
  `tscloudvpn-vol-{name}`, reconstruct the volumes map. No local state needed.

Config addition:

```yaml
volumes:
  cloudflare:
    account_id: "..."
    api_token: "..."       # master token
    bucket: "tscloudvpn-volumes"
```

### 0c: File server service

A minimal service that validates the full stack:

- Service type: `files`
- Init script: tailscale + cloudnbd mount + filebrowser (single Go binary)
- VM requirements: cheapest (same as exit node)
- Persistence: 10G volume at `/data`
- Idle detection: no HTTP requests for 2 hours
- Named instances: `files-photos`, `files-docs`, etc.

Init script installs filebrowser from a pinned GitHub release URL. No nix yet.

**Validates:**
- Service type abstraction works end-to-end
- cloudnbd mounts and persists data across VM lifecycles
- R2 credential scoping and revocation works
- Data survives provider/region changes
- Named instances work
- Different idle detection from exit nodes

### 0d: UI changes

- Service type selector in the UI (tabs or dropdown)
- For persistent services: volume name input field
- Running nodes show service-specific info (e.g., file server shows a link to
  the web UI on the tailnet)
- API routes include service type

---

## Phase 1: Android emulator

**Goal:** A more demanding service that exercises VM requirements filtering
(nested virt, larger instances) and persistent environments.

### 1a: VM requirements filtering

- Providers report capability (nested virt support)
- `VMRequirements.NestedVirt` filters providers that don't support it
- VM size selection picks cheapest instance meeting all requirements
- Provider compatibility:
  - GCP: yes (--enable-nested-virtualization)
  - AWS: yes (C8i/M8i/R8i or bare metal)
  - Azure: yes (v5+ series)
  - DigitalOcean: yes (KVM on all droplets)
  - Hetzner Cloud: no (dedicated only)
  - Vultr/Linode: unclear, needs testing

### 1b: Nix-built environment tarballs

- `flake.nix` in the repo defines the emulator environment:
  - JDK 17 (Temurin)
  - Android SDK command-line tools
  - Android emulator
  - System image (google_atd x86_64)
  - Platform tools (adb)
- `nix build .#android-emu` produces a hermetic closure
- Export as tarball, upload to R2 under `envs/` prefix
- Versioned: `android-emu-v1.tar.zst`
- `ServiceType.Env` carries URL + SHA256 checksum

The VM init script downloads the tarball to persistent storage on first boot.
Subsequent boots just set PATH. No apt, no drift.

### 1c: Emulator init script

Phases:
1. Tailscale join (shared template)
2. cloudnbd mount (shared template)
3. Download nix-built environment tarball (if not cached on persistent volume)
4. Create AVD (first boot only)
5. Start emulator headless (`-no-window -gpu swiftshader_indirect`)
6. Enable ADB over network on tailscale IP
7. Stats server (idle = no ADB clients + low CPU for N hours)

### 1d: Emulator-specific UI

- Show ADB connection string: `adb connect emu-myphone.tailnet:5555`
- Show scrcpy command: `scrcpy -s emu-myphone.tailnet:5555`
- Show emulator status (booting / ready / idle)

---

## Phase 2: Dev VM

**Goal:** SSH-accessible development environment with persistent home directory
and user-customizable tool selection.

### Open questions

- How does the user specify which tools they want? Options:
  - A `flake.nix` in their own repo that defines the dev environment
  - Configuration in tscloudvpn that selects from a menu of tool sets
  - A `devvm.nix` file in the persistent volume itself (self-describing)
- How do dotfiles get provisioned? (git clone of dotfiles repo on first boot?)
- SSH key management (already handled by tscloudvpn config's SSH public key)
- Shell customization (zsh vs bash, which plugins)

### Likely approach

- Base environment via nix tarball (git, build tools, common utilities)
- User-specific customization via a nix flake ref stored in the volume config
  or tscloudvpn config
- Persistent `/home` directory via cloudnbd
- SSH access over tailnet (tailscale SSH or standard SSH on tailscale IP)
- Idle detection: no SSH sessions for N hours
- Possibly code-server (VS Code in browser) as an optional addition

---

## Phase 3+: Future services

Services that follow the same pattern and can be added incrementally:

| Service | Persistence | VM reqs | Notes |
|---------|------------|---------|-------|
| SOCKS5 proxy | No | Cheapest | Lighter than exit node, per-app routing |
| Game server (Minecraft, etc.) | Yes | Medium | Well-known idle detection (no players) |
| Jupyter notebook | Yes | Medium-large | Nix env with Python/conda |
| Subnet router | No | Cheapest | Ephemeral access to cloud VPC |
| Database sandbox | Yes | Medium | Persistent PostgreSQL data dir |
| Build server | Yes | Large | Persistent build cache (ccache, cargo) |
| LLM inference | Yes | GPU instance | Persistent model weights |

---

## Future optimizations (not blocking any phase)

### On-demand startup

- `GET /services/{service}/{name}/ensure` endpoint — starts VM if not running,
  blocks until ready, returns VM address.
- For HTTP services: tscloudvpn serves a "starting..." landing page, redirects
  when ready.
- For SSH: `ProxyCommand` calls the ensure endpoint.
- Not needed for v0-v2. Deliberate create via UI is fine.

### Pre-baked VM images

- Build custom VM images per provider with tailscale + cloudnbd pre-installed.
- Eliminates apt-get on boot, reduces startup time to ~30s.
- Maintenance burden: 7 providers x N service types. Consider only for
  high-frequency services.

### Squashfs environment images

- Build nix closure as a squashfs image instead of a tarball.
- Mount read-only via cloudnbd (a second block device).
- Lazy loading: only blocks actually accessed are fetched from R2.
- Zero extraction time. Two cloudnbd mounts per VM: one RO env, one RW data.

### Container-based services

- Instead of full VMs, run services as containers on a persistent host or
  on a container platform (Fly.io, etc.).
- Faster startup (seconds vs minutes).
- Different provider abstraction needed.

---

## Implementation notes

### Init script template structure

Shared phases extracted into composable templates:

```
{{template "base-tailscale" .}}     ← all services
{{template "base-cloudnbd" .}}      ← persistent services
{{template "base-nix-env" .}}       ← services with nix environments
{{service-specific setup}}
{{template "base-stats-server" .}}  ← all services
```

### Registry key format

`{service}-{name}-{provider}-{region}` where name is optional (exit nodes
have no name). Examples:
- `exit-do-nyc1`
- `files-photos-gcp-us-central1`
- `emu-myphone-ec2-us-west-2`

### Graceful shutdown for persistent services

1. Signal the VM to unmount (SSH command or init script trap)
2. cloudnbd flushes pending writes to R2
3. Revoke R2 token (instant fencing)
4. Delete VM from cloud provider
5. Remove device from Tailscale

### Stats endpoint contract

All services expose the same JSON structure on their stats path:

```json
{"last_active": <unix_timestamp>, ...service-specific fields...}
```

The `last_active` field is the universal idle detection signal.
`ServiceType.ParseStats` can extract additional service-specific metrics
for display in the UI, but idle shutdown only looks at `last_active`.

---

## Phase 0a implementation detail

All work on branch `service-runner`.

### Provider interface — before and after

Before:
```go
type Provider interface {
    CreateInstance(ctx context.Context, region string, key *controlapi.PreauthKey) (Instance, error)
    DeleteInstance(ctx context.Context, instanceID Instance) error
    GetInstanceStatus(ctx context.Context, region string) (InstanceStatus, error)
    ListInstances(ctx context.Context, region string) ([]Instance, error)
    ListRegions(ctx context.Context) ([]Region, error)
    Hostname(region string) HostName
    GetRegionHourlyEstimate(region string) float64
}
```

After:
```go
type CreateRequest struct {
    Region       string
    Hostname     string         // fully formed: "exit-do-nyc1"
    UserData     string         // rendered init script
    Requirements VMRequirements // from ServiceType (zero value = cheapest)
}

type Provider interface {
    CreateInstance(ctx context.Context, req CreateRequest) (Instance, error)
    DeleteInstance(ctx context.Context, instanceID Instance) error
    GetInstanceStatus(ctx context.Context, region string) (InstanceStatus, error)
    ListInstances(ctx context.Context, region string) ([]Instance, error)
    ListRegions(ctx context.Context) ([]Region, error)
    Hostname(region string, servicePrefix string) HostName
    GetRegionHourlyEstimate(region string, reqs VMRequirements) float64
}
```

Key changes:
- `CreateInstance` takes `CreateRequest`. Provider no longer templates the init
  script or generates the hostname — both come from the caller.
- `Hostname` takes a `servicePrefix` so it can produce `exit-do-nyc1`.
- `GetRegionHourlyEstimate` takes `VMRequirements`. For Phase 0a all providers
  ignore the requirements and return cheapest price (same as today). Becomes
  meaningful in Phase 1 when the emulator needs larger instances.

### ControlApi.CreateKey — before and after

Before:
```go
CreateKey(ctx context.Context) (*PreauthKey, error)
// Hardcodes tags: []string{"tag:untrusted"}
```

After:
```go
CreateKey(ctx context.Context, tags []string) (*PreauthKey, error)
// Caller passes svc.Tags
```

### Changes per file

**internal/services/service.go** (NEW package)
- `ServiceType`, `VMRequirements`, `PersistenceConfig` structs
- `var ExitNode` definition with current exit-node behavior
- `parseExitNodeStats([]byte) (time.Time, error)` — extracted from tsclient
- `RenderInitScript(svc ServiceType, hostname, tailscaleArgs, sshKey string) (string, error)`
  helper that templates the init script

**internal/providers/provider.go**
- Add `CreateRequest` struct (references `services.VMRequirements` or define
  locally — TBD whether to avoid circular imports)
- Update `Provider` interface signatures
- `InitData` embed remains (ExitNode's InitScript field references it)
- Providers no longer reference `InitData` directly

**internal/providers/{do,ec2,gcp,azure,hetzner,linode,vultr}/**
Per provider — mechanical changes:
1. `CreateInstance(ctx, req CreateRequest)` — remove template execution block,
   use `req.UserData`. Remove hostname generation, use `req.Hostname`.
   VM size selection unchanged (cheapest). `req.Requirements` available but
   ignored when zero-value.
2. `Hostname(region, servicePrefix string)` — returns
   `HostName(servicePrefix + "-" + providerShortName + "-" + region)`.
3. `GetRegionHourlyEstimate(region, VMRequirements)` — ignores requirements,
   returns cheapest (same behavior as today).

**internal/providers/fake/fake.go**
Same interface changes. Uses `req.Hostname` directly. No template logic to remove.

**internal/controlapi/controlapi.go**
`CreateKey(ctx, tags []string)` signature.

**internal/controlapi/tailscale.go**
Use `tags` parameter in `capabilities.Devices.Create.Tags` instead of
hardcoded `tag:untrusted`.

**internal/controlapi/headscale.go**
Use `tags` parameter similarly.

**internal/instances/instances.go** (Creator)
`Create()` takes `*services.ServiceType` and `sshKey string`:
1. `controlApi.CreateKey(ctx, svc.Tags)`
2. `services.RenderInitScript(svc, hostname, key.GetCLIArgs(), sshKey)`
3. Build `CreateRequest{Region, Hostname, UserData, Requirements}`
4. `provider.CreateInstance(ctx, req)`

**internal/instances/controller.go**
- New field: `serviceType *services.ServiceType`
- `NewController` takes `*services.ServiceType` parameter
- `shouldIdleShutdown()` uses `c.serviceType.IdleTimeout` (0 = no idle shutdown)
- `fetchStats()` uses `c.serviceType.StatsPath` (skip if "")
- Stats parsing uses `c.serviceType.ParseStats` (skip if nil)
- `monitorHealth()` gets hostname via `provider.Hostname(region, svc.Name)`

**internal/instances/registry.go**
- `CreateInstance(ctx, providerName, region string, svc *services.ServiceType)`
- `DeleteInstance(providerName, region string, svc *services.ServiceType)`
- `GetInstanceStatus(providerName, region string, svc *services.ServiceType)`
- Registry key: `fmt.Sprintf("%s-%s-%s", svc.Name, providerName, region)`
- `discoverInstances` matches hostnames against known service name prefixes
  (for Phase 0a, only "exit-" is recognized)

**internal/instances/gc.go**
- Hostname matching updated for `{service}-{provider}-{region}` format

**internal/server/manager.go**
- Routes: `/providers/{provider}/services/{service}/regions/{region}` for
  PUT/DELETE. Keep old routes as aliases pointing to "exit" service for
  backward compat during transition.
- `GetStatus` includes service type in mappedRegion
- SSE event keys include service prefix

**internal/app/e2e_test.go**
- `MockControlAPI.CreateKey` takes `tags` parameter
- `TestHarness.CreateInstance` / `DeleteInstance` URL paths updated
- Hostname expectations updated (e.g., `"exit-fake-fake-us-east"`)

**internal/app/scenario_test.go**
- Same hostname changes

**internal/instances/controller_test.go**
- `MockProvider` updated for new interface
- `MockControlApi.CreateKey` takes tags
- Controller tests pass ServiceType

### Implementation order

1. ✅ Create branch `service-runner`
2. Add `internal/services/service.go` — ServiceType + ExitNode + helpers
3. Update `Provider` interface + `CreateRequest` in provider.go
4. Update `ControlApi.CreateKey` signature + implementations
5. Update each provider impl (7 real + fake) — mechanical change
6. Update Creator to use ServiceType for templating
7. Update Controller to hold ServiceType, use idle/stats config
8. Update Registry — key format, method signatures
9. Update GC — hostname matching
10. Update Manager + routes
11. Fix all test files
12. Run full test suite, verify zero regression

### Commit sequence

Each commit is self-contained and passes all tests.

- [ ] **Commit 1: Extract init script rendering into shared helper.**
  Pure refactor. All 7 providers do identical template.Execute logic. Extract
  into `providers.RenderInitScript(initScript, hostname, key, sshKey)`. Each
  provider calls it instead of duplicating. No interface change.

- [ ] **Commit 2: Extract hostname generation into shared helper.**
  Pure refactor. Each provider has its own `fooInstanceHostname(region)` that
  does `"{prefix}-" + region`. Replace with `providers.MakeHostname(prefix, region)`.
  No interface change.

- [ ] **Commit 3: Add `internal/services` package with ServiceType + ExitNode.**
  Additive only — no existing code modified. Struct definitions, `var ExitNode`,
  `parseExitNodeStats`. No callers yet. Nothing can break.

- [ ] **Commit 4: ControlApi.CreateKey accepts tags.**
  `CreateKey(ctx)` → `CreateKey(ctx, tags []string)`. Update tailscale.go,
  headscale.go, both mocks. All callers pass `[]string{"tag:untrusted"}` —
  identical behavior.

- [ ] **Commit 5: Provider.CreateInstance takes CreateRequest.**
  Payoff from commits 1-2. Per-provider diff is small: receive `req CreateRequest`
  instead of `(region, key)`, use `req.UserData` and `req.Hostname` instead of
  calling the shared helpers. Creator builds the CreateRequest. Fake provider +
  mocks updated.

- [ ] **Commit 6: Provider.Hostname takes servicePrefix.**
  `Hostname(region)` → `Hostname(region, servicePrefix)`. All callers pass
  `"exit"` for now. Hostnames change from `do-nyc1` to `exit-do-nyc1`. Tests
  updated to expect new format.

- [ ] **Commit 7: Provider.GetRegionHourlyEstimate takes VMRequirements.**
  Small signature change. All providers ignore the parameter and return cheapest.
  Callers pass zero value.

- [ ] **Commit 8: Thread ServiceType through Controller.**
  Controller holds `*ServiceType`. Uses `svc.IdleTimeout` instead of hardcoded
  4h constant. Uses `svc.StatsPath` and `svc.ParseStats`. `NewController` takes
  the extra parameter. All callers pass `&services.ExitNode`.

- [ ] **Commit 9: Thread ServiceType through Registry.**
  Registry key becomes `{service}-{provider}-{region}`. Method signatures take
  `*ServiceType`. Discovery matches hostname prefix. GC updated.

- [ ] **Commit 10: Update Manager + API routes.**
  Routes include service type. SSE events include service prefix. UI shows
  service label.

### Hostname migration

Existing running exit nodes have hostnames like `do-nyc1`. After this change,
new ones will be `exit-do-nyc1`. Running nodes won't be discovered after the
update (old hostname doesn't match new pattern). Since exit nodes are ephemeral
and cheap, this is acceptable — they'll idle-shutdown or be GC'd, and new
creates will use the new format.

### ZFS for persistent services

All persistent services use ZFS instead of ext4. This adds DKMS compilation
overhead at first boot of a new kernel but provides:
- ZFS ARC for read caching (helps paper over S3/R2 latency)
- Readahead optimization
- Snapshots, compression, checksumming

The `PersistenceConfig.FSType` is always `"zfs"` for persistent services.
