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

- **Hostnames are user-friendly, not parsed for identity.**
  - Exit nodes: `do-nyc1` (unchanged — region IS the identity)
  - Named services: user-chosen, e.g. `pixel`, `myproject`, `work-backup`
  - No service prefix required. Hostname is for humans, not machines.

- **Discovery via Tailscale tags + stats endpoint.**
  - All tscloudvpn-managed nodes get a shared tag (e.g., `tag:tscloudvpn`),
    defined in `ServiceType.Tags`.
  - Discovery filters Tailscale peers by tag (skips untagged personal devices).
  - The stats/identity endpoint on port 8245 reports full identity:
    service type, provider, region, name.
  - No hostname parsing needed. No local state needed.
  - Per-service tags (e.g., `tag:tscloudvpn-exit`) enable per-service ACL
    policies like autoApprovers for exit routes.

- **Stats/identity endpoint on port 8245.** All managed VMs serve a JSON
  endpoint at `http://{tailscale-ip}:8245/stats.json` with identity +
  metrics. Port 80 is left free for the actual service (filebrowser,
  Jupyter, code-server, etc.).

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

### Stats/identity endpoint contract

All managed VMs serve `http://{tailscale-ip}:8245/stats.json` with:

```json
{
  "identity": {
    "service": "exit",
    "provider": "do",
    "region": "nyc1",
    "name": ""
  },
  "last_active": <unix_timestamp>,
  ...service-specific fields (forwarded_bytes, adb_clients, etc.)...
}
```

The `identity` block is static (written at boot from init script template
vars). It is the authoritative source for mapping a Tailscale peer to a
service type, provider, and region — no hostname parsing needed.

The `last_active` field is the universal idle detection signal.
`ServiceType.ParseStats` can extract additional service-specific metrics
for display in the UI, but idle shutdown only looks at `last_active`.

Port 8245 is a fixed convention. Port 80 is left free for the service itself.

---

## Phase 0a implementation detail

All work on branch `service-runner`.

### Interface changes done so far

**Provider interface** ✅ done
```go
type CreateRequest struct {
    Region   string
    UserData string // Rendered init script
    SSHKey   string // SSH public key (Azure OS profile)
}

type Provider interface {
    CreateInstance(ctx context.Context, req CreateRequest) (Instance, error)
    // ... rest unchanged
}
```

`Hostname` and `GetRegionHourlyEstimate` signatures are NOT changing.
Hostnames are for humans, not identity. Discovery uses tags + stats endpoint.

**ControlApi.CreateKey** ✅ done
```go
CreateKey(ctx context.Context, tags []string) (*PreauthKey, error)
```

### Controller refactor (next)

Today, Controller is a God object: state tracker + health monitor + instance
creator + instance deleter. It holds `controlApi`, `provider`, `sshKey`,
`serviceType` — most of which it only uses once (at creation time) and then
passes through.

Target: Controller becomes purely state + health monitor. Registry handles
instance creation and deletion directly (it already has all the context).

**Before:**
```
Registry.CreateInstance(providerName, region)
  → creates Controller(provider, region, sshKey, serviceType, controlApi, tsClient)
  → Controller.Create()
    → Creator.Create(controlApi, provider, region)  // Controller passes through
```

**After:**
```
Registry.CreateInstance(providerName, region)
  → renders init script, calls controlApi.CreateKey, provider.CreateInstance
  → creates Controller(hostname, serviceType, tsClient)  // monitor only
```

Controller loses: `provider`, `controlApi`, `sshKey`, `Creator`.
Controller keeps: `serviceType` (idle timeout, stats), `tsClient` (pings),
  hostname, state, ping history.

Deletion moves similarly: Registry calls `controlApi.DeleteDevice` and
`provider.DeleteInstance` directly, then calls `controller.Stop()`.

### Changes per file — remaining

**internal/instances/controller.go** — next commit
- Remove `provider`, `controlApi`, `sshKey` fields
- Remove `Create()` and `Delete()` methods
- Constructor takes hostname + serviceType + tsClient only
- Pure health monitor: ping loop, stats fetch, idle detection

**internal/instances/instances.go** — next commit
- Creator called by Registry directly, not by Controller
- Or inline creation logic into Registry (Creator may become unnecessary)

**internal/instances/registry.go** — next commit
- `CreateInstance` does: render init script → create key → create instance
  → create Controller for monitoring
- `DeleteInstance` does: delete from Tailscale → delete from cloud → stop Controller

**internal/providers/install.sh.tmpl** — identity endpoint commit
- Port 80 → 8245, write identity.json at boot, stats.json stays separate

**internal/tsclient/** — identity endpoint commit
- Port 8245 in FetchNodeStats URL
- Add FetchNodeIdentity method + NodeIdentity struct

**internal/server/manager.go** — later
- Routes include service type, UI shows service label

### Commit sequence

Each commit is self-contained and passes all tests.

- [x] **Commit 1: Move init script rendering into Creator, introduce CreateRequest.**
  `Provider.CreateInstance` now accepts `CreateRequest{Region, UserData, SSHKey}`
  instead of raw parameters. Creator renders the init script via
  `providers.RenderUserData` and builds the CreateRequest. SSH key threaded
  through App → Server → Manager → Registry → Controller → Creator. Dead
  `sshKey` fields removed from all providers (Azure now uses `req.SSHKey`).

- [x] **Commit 2: Add `internal/services` package with ServiceType + ExitNode.**
  Additive only — no existing code modified. Struct definitions, `var ExitNode`,
  `parseExitNodeStats`. No callers yet.

- [x] **Commit 3: Thread ServiceType through Controller and Creator.**
  Controller holds `*ServiceType`, uses `svc.IdleTimeout` instead of hardcoded
  4h constant. Creator takes `*ServiceType`, uses `svc.Tags` for CreateKey.
  `ControlApi.CreateKey` now accepts `tags []string`. Only `app.go` (tsnet
  server) keeps hardcoded `tag:untrusted`.

- [x] **Commit 4: Move instance creation/deletion from Controller to Registry.**
  Controller becomes purely state + health monitor. Registry handles the full
  create flow (render init script → create key → create cloud instance) and
  delete flow (delete Tailscale device → delete cloud instance). Controller
  loses `provider`, `controlApi`, `sshKey`, `Creator`. Creator eliminated.
  Start()/Stop() separated so Stop() is safe before Start() (fixes deadlock
  on creation failure). Race on `started` field fixed with mutex.

- [x] **Commit 5: Stats/identity endpoint on port 8245.**
  Init script writes static identity.json at boot with service, provider,
  region. Stats HTTP server moved from port 80 to 8245. RenderUserData
  accepts identity parameters. FetchNodeStats URL updated to port 8245.

- [x] **Commit 6: Tag-based discovery.**
  Discovery filters control API devices by service tags. For tagged
  devices without a controller, fetches the identity endpoint
  (`/identity.json` on port 8245) to learn service/provider/region.
  Replaces hostname-matching discovery (no more iterating all
  provider-region combos). `FetchNodeIdentity` added to
  `TailscaleClient` interface. Headscale `ListDevices` fixed to use
  `GetForcedTags()` instead of `GetGivenName()` for device tags.

- [x] **Commit 7: Thread ServiceType through Registry + Manager.**
  Registry key becomes `{service}-{provider}-{region}`. Controllers
  wrapped in `controllerEntry` with service/provider/region metadata
  (Controller itself stays a pure health monitor). API routes become
  `/services/{service}/providers/{provider}/regions/{region}`. SSE event
  keys include service. Node cards show service label badge. Registry
  methods (`CreateInstance`, `DeleteInstance`, `GetInstanceStatus`) all
  take service name. `GetAllInstanceStatuses` populates
  Service/Provider/Region from stored metadata instead of parsing keys.

### Phase 0b+0c implementation sequence

Validated end-to-end with a spike (stashed on `service-runner` branch).
cloudblockdev + ZFS + filebrowser + R2 persistence works across VM
lifecycles. Boot time ~1.5min on Ubuntu (vs ~7min on Debian due to ZFS
DKMS compilation). Landing as focused commits:

- [x] **Commit 8: Switch all providers to Ubuntu 24.04.**
  All 7 providers currently use Debian 12. Switch to Ubuntu 24.04:
  EC2 SSM path, DO slug, Vultr OS filter, Linode image ID, Hetzner
  image name, GCP image family, Azure publisher/offer/SKU. Update
  exit node init script tailscale repo URLs (`ubuntu/noble` instead
  of `debian/bookworm`). Ubuntu ships ZFS as a prebuilt kernel module
  — no DKMS needed.

- [ ] **Commit 9: Init script rendering refactor.**
  `RenderUserData` takes init script template as parameter (currently
  hardcodes `InitData`). Use `svcType.InitScript` in registry's
  `createCloudInstance`. Extend `InitScriptData` with R2 credential
  fields (`R2AccessKeyID`, `R2SecretAccessKey`, `R2Endpoint`,
  `R2Bucket`, `VolumeSize`, `VolumeName`). Add
  `RenderUserDataWithPersistence` that accepts a pre-populated
  `InitScriptData`. No behavior change for exit nodes.

- [ ] **Commit 10: Named instances in registry.**
  Add `instanceName` parameter to `CreateInstance`, `DeleteInstance`,
  `GetInstanceStatus`. `registryKey` returns `{service}-{name}` for
  persistent services (name non-empty), `{service}-{provider}-{region}`
  for ephemeral. Add `name` field to `controllerEntry`, `InstanceName`
  to `InstanceStatus`. All existing callers pass `""`.

- [ ] **Commit 11: Cloudflare config + R2 client.**
  New `internal/r2/r2.go`: `TokenManager` with `CreateVolume` (create
  bucket + scoped API token, derive S3 creds via SHA-256 of token
  value) and `RevokeToken` (find token by name, delete it). One R2
  bucket per volume (`tscloudvpn-vol-{name}`). Permission group IDs
  looked up at runtime via `GET /user/tokens/permission_groups` and
  cached. Resource scope: `com.cloudflare.edge.r2.bucket.{acct}_default_{bucket}`.
  Config: `cloudflare.account_id` + `cloudflare.api_token`. Wired
  through app → server → manager → registry (nil if not configured).

- [ ] **Commit 12: R2 wiring in registry lifecycle.**
  In `createCloudInstance`: if `svcType.Persistence != nil` and R2 is
  configured, call `CreateVolume` then `RenderUserDataWithPersistence`
  with credentials. In `DeleteInstance`: revoke R2 token before
  deleting from control plane/cloud (instant fencing). Bucket and
  data are preserved for future resume.

- [ ] **Commit 13: File server ServiceType + init script.**
  `var FileServer = ServiceType{Name: "files", Label: "File Server", ...}`
  with `Persistence: &PersistenceConfig{MountPoint: "/data", Size: "10G"}`,
  `IdleTimeout: 2h`, `Tags: ["tag:untrusted"]`. Init script
  (`fileserver_init.sh.tmpl`, embedded): Phase 1: modprobe nbd,
  download cloudblockdev (arch-aware amd64/arm64 from GitHub release
  1772439544-6ebe4db), write YAML config with R2 creds, start
  cloudblockdev in background. Phase 2: install tailscale + ZFS,
  join tailnet. Phase 3: wait for /dev/nbd0, `zpool import -f data`
  or `zpool create` for new volumes. Phase 4: download filebrowser
  (v2.61.0), init DB, run on port 80 bound to tailscale IP. Phase 5:
  stats server on 8245, idle detection via filebrowser I/O activity.
  Add to `services.All` catalog.

- [ ] **Commit 14: API routes + minimal UI.**
  Routes: `PUT /services/files/instances/{name}/providers/{provider}/regions/{region}`
  and `DELETE /services/files/instances/{name}` (looks up provider/region
  from running instance status). UI: "Launch File Server" section with
  name text input, provider/region dropdown, create button. Uses
  `fetch()` (not HTMX) since the form is different from exit nodes.

- [ ] **Commit 15: Cleanup.**
  Cloud provider instance tags include service type. GC handles new
  registry key format. identity.json includes instance name for
  discovery of persistent services. Restore init script ERR/EXIT
  trap. Improve async error reporting in UI.

### ZFS for persistent services

Ubuntu ships ZFS as a prebuilt kernel module in `linux-modules-extra`.
No DKMS compilation needed — `apt install zfsutils-linux` + `modprobe zfs`
works immediately. ZFS provides:
