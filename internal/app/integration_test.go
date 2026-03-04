// Package app_test contains end-to-end integration tests for tscloudvpn.
//
// These tests create real cloud instances across all configured providers,
// wait for them to register with the Tailscale/Headscale control plane, and
// verify that each exit node can actually forward traffic by routing through
// it from a dedicated Docker container on the same tailnet.
//
// Requirements:
//   - Build tag 'e2e'
//   - REAL_INTEGRATION_TEST=true environment variable
//   - Tailscale/Headscale control plane credentials
//   - Cloud provider credentials for all registered providers
//   - Docker available for traffic verification
//   - Will incur actual costs from cloud providers

//go:build e2e

package app_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	crypto_rand "crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/anupcshan/tscloudvpn/internal/app"
	"github.com/anupcshan/tscloudvpn/internal/config"
	"github.com/anupcshan/tscloudvpn/internal/controlapi"
	"github.com/anupcshan/tscloudvpn/internal/providers"
	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	// Import all cloud provider implementations to register them
	_ "github.com/anupcshan/tscloudvpn/internal/providers/azure"
	_ "github.com/anupcshan/tscloudvpn/internal/providers/digitalocean"
	_ "github.com/anupcshan/tscloudvpn/internal/providers/ec2"
	_ "github.com/anupcshan/tscloudvpn/internal/providers/gcp"
	_ "github.com/anupcshan/tscloudvpn/internal/providers/hetzner"
	_ "github.com/anupcshan/tscloudvpn/internal/providers/linode"
	_ "github.com/anupcshan/tscloudvpn/internal/providers/vultr"
)

// defaultTestRegions maps provider names to cheap default regions.
// Override with E2E_<PROVIDER>_REGION environment variables.
var defaultTestRegions = map[string]string{
	"do":      "sfo3",
	"vultr":   "lax",
	"linode":  "us-lax",
	"ec2":     "us-west-2",
	"gcp":     "us-west1",
	"hetzner": "hil",
	"azure":   "westus2",
}

// regionEnvVars maps provider names to the environment variable that overrides the test region.
var regionEnvVars = map[string]string{
	"do":      "E2E_DO_REGION",
	"vultr":   "E2E_VULTR_REGION",
	"linode":  "E2E_LINODE_REGION",
	"ec2":     "E2E_AWS_REGION",
	"gcp":     "E2E_GCP_REGION",
	"hetzner": "E2E_HETZNER_REGION",
	"azure":   "E2E_AZURE_REGION",
}

// TestExitNode launches an exit node on each registered cloud provider,
// waits for it to register with the control plane, and verifies traffic
// forwarding by routing through it from a Docker container on the same tailnet.
//
// Each provider is a subtest, so individual providers can be tested with:
//
//	go test -tags=e2e -run /ec2
func TestExitNode(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping real integration tests in short mode")
	}
	if os.Getenv("REAL_INTEGRATION_TEST") != "true" {
		t.Skip("Skipping real integration test - set REAL_INTEGRATION_TEST=true to enable")
	}

	cfg, err := loadRealConfig(t)
	require.NoError(t, err, "Failed to load real configuration")

	if !hasControlAPIConfigured(cfg) {
		t.Skip("No Tailscale/Headscale credentials configured")
	}

	application, err := app.New("")
	require.NoError(t, err, "Failed to create app")

	initCtx, initCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer initCancel()

	require.NoError(t, application.Initialize(initCtx), "Failed to initialize app")

	controller, err := cfg.GetController()
	require.NoError(t, err, "Failed to get controller")

	pool, err := dockertest.NewPool("")
	require.NoError(t, err, "Failed to connect to Docker")
	require.NoError(t, pool.Client.Ping(), "Docker is not reachable")

	// One subtest per registered provider (skip the fake test provider).
	for name := range providers.ProviderFactoryRegistry {
		if name == "fake" {
			continue
		}
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()

			provider := application.GetCloudProvider(name)
			require.NotNil(t, provider, "Provider %s has no credentials configured", name)

			region := getTestRegion(name)
			require.NotEmpty(t, region, "No test region for provider %s", name)

			// Start a dedicated Docker test client for this provider.
			testClient := startTestClient(t, ctx, pool, controller, cfg, name)

			originalIP := dockerFetchURL(t, testClient, "https://api.ipify.org")
			require.NotEmpty(t, originalIP, "Failed to get test client's external IP")
			t.Logf("Test client original external IP: %s", originalIP)

			// Generate ephemeral SSH key for this test.
			sshSigner, sshPubKey := generateTestSSHKey(t)

			// Create auth key and instance.
			key, err := controller.CreateKey(ctx, []string{"tag:untrusted"})
			require.NoError(t, err, "Failed to create auth key")

			hostname := string(provider.Hostname(region))
			userData, err := providers.RenderUserData(hostname, key, sshPubKey, "exit", name, region, true)
			require.NoError(t, err, "Failed to render user data")

			t.Logf("Creating %s exit node in %s", name, region)
			instance, err := provider.CreateInstance(ctx, providers.CreateRequest{
				Region:   region,
				UserData: userData,
				SSHKey:   sshPubKey,
				Debug:    true,
			})
			require.NoError(t, err, "Failed to create instance")
			t.Logf("Created instance: %s (provider ID: %s)", instance.Hostname, instance.ProviderID)

			// Clean up instance and device when done.
			t.Cleanup(func() {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 3*time.Minute)
				defer cleanupCancel()
				t.Logf("Cleaning up %s instance %s", name, instance.ProviderID)
				if err := provider.DeleteInstance(cleanupCtx, instance); err != nil {
					t.Logf("Failed to delete %s instance: %v", name, err)
				}
				if dev, err := findDeviceByHostname(cleanupCtx, controller, instance.Hostname); err == nil && dev != nil {
					if err := controller.DeleteDevice(cleanupCtx, dev); err != nil {
						t.Logf("Failed to delete device %s: %v", dev.Name, err)
					}
				}
			})

			// Start streaming cloud-init output in the background. Retries
			// IP lookup + SSH connect until the VM is reachable, then streams
			// tail -f. Stops when tailCancel is called or the VM is deleted.
			tailCtx, tailCancel := context.WithCancel(context.Background())
			t.Cleanup(tailCancel)
			if getter, ok := provider.(providers.PublicIPGetter); ok {
				go sshTailCloudInitLog(tailCtx, t, getter, instance, sshSigner)
			}

			// Wait for the instance to register with the control plane.
			t.Logf("Waiting for %s to register with control plane...", hostname)
			device, err := waitForDeviceRegistration(ctx, controller, instance.Hostname, 3*time.Minute)
			if err != nil {
				// Let the tail stream a bit longer before we fail — the log
				// output above will show what went wrong.
				require.NoError(t, err, "Timed out waiting for device registration (see cloud-init log above)")
			}
			require.True(t, device.IsOnline, "Device is not online")
			require.NotEmpty(t, device.IPAddrs, "Device has no IP addresses")
			t.Logf("Device registered: %s, IPs: %v", device.Name, device.IPAddrs)

			exitNodeIP := device.IPAddrs[0]

			// Verify stats and identity endpoints before setting exit node.
			statsURL := fmt.Sprintf("http://%s:8245/stats.json", exitNodeIP)
			var statsBody string
			require.Eventually(t, func() bool {
				statsBody = dockerFetchURL(t, testClient, statsURL)
				return statsBody != ""
			}, 3*time.Minute, 10*time.Second, "Stats endpoint not reachable at %s", statsURL)
			require.Contains(t, statsBody, "forwarded_bytes", "Stats response missing forwarded_bytes: %s", statsBody)
			require.Contains(t, statsBody, "last_active", "Stats response missing last_active: %s", statsBody)
			t.Logf("Stats endpoint verified: %s", statsBody)

			identityURL := fmt.Sprintf("http://%s:8245/identity.json", exitNodeIP)
			identityBody := dockerFetchURL(t, testClient, identityURL)
			require.NotEmpty(t, identityBody, "Identity endpoint not reachable at %s", identityURL)

			var identity struct {
				Service  string `json:"service"`
				Provider string `json:"provider"`
				Region   string `json:"region"`
			}
			require.NoError(t, json.Unmarshal([]byte(identityBody), &identity), "Invalid identity JSON: %s", identityBody)
			require.Equal(t, "exit", identity.Service, "Wrong service in identity")
			require.Equal(t, name, identity.Provider, "Wrong provider in identity")
			require.Equal(t, region, identity.Region, "Wrong region in identity")
			t.Logf("Identity endpoint verified: %s", identityBody)

			// Wait for the exit node to advertise routes, then set it.
			t.Logf("Waiting for %s to advertise exit node routes (IP: %s)", hostname, exitNodeIP)

			require.Eventually(t, func() bool {
				var stdout bytes.Buffer
				code, _ := testClient.Exec(
					[]string{"tailscale", "set", "--exit-node=" + exitNodeIP},
					dockertest.ExecOptions{StdOut: &stdout, StdErr: &stdout},
				)
				if code != 0 {
					t.Logf("tailscale set --exit-node=%s failed: %s", exitNodeIP, strings.TrimSpace(stdout.String()))
				}
				return code == 0
			}, 3*time.Minute, 10*time.Second, "Exit node %s never started advertising", hostname)

			var routedIP string
			require.Eventually(t, func() bool {
				routedIP = dockerFetchURL(t, testClient, "https://api.ipify.org")
				t.Logf("External IP check: got %q (original: %s)", routedIP, originalIP)
				return routedIP != "" && routedIP != originalIP
			}, 3*time.Minute, 5*time.Second, "External IP did not change (still %s)", originalIP)

			t.Logf("Traffic verified: original IP %s -> routed IP %s (via %s/%s)", originalIP, routedIP, name, region)
		})
	}
}

// startTestClient creates a Docker container running tailscale, joins it to the
// tailnet, and returns the dockertest resource. Each provider gets its own
// container so exit node routing tests run fully in parallel.
// The container is cleaned up automatically via t.Cleanup.
func startTestClient(t *testing.T, ctx context.Context, pool *dockertest.Pool, controller controlapi.ControlApi, cfg *config.Config, providerName string) *dockertest.Resource {
	t.Helper()

	key, err := controller.CreateKey(ctx, []string{"tag:untrusted"})
	require.NoError(t, err, "Failed to create auth key for test client")

	clientHostname := "e2e-test-" + providerName
	tsArgs := []string{"--hostname=" + clientHostname}
	tsArgs = append(tsArgs, key.GetCLIArgs()...)

	resource, err := pool.RunWithOptions(&dockertest.RunOptions{
		Repository: "tailscale/tailscale",
		Tag:        "stable",
		Env:        []string{"TS_STATE_DIR=/var/lib/tailscale"},
		Cmd:        []string{"tailscaled", "--state=/var/lib/tailscale/tailscaled.state"},
	}, func(hc *docker.HostConfig) {
		hc.CapAdd = []string{"NET_ADMIN", "NET_RAW"}
		hc.Devices = []docker.Device{
			{PathOnHost: "/dev/net/tun", PathInContainer: "/dev/net/tun", CgroupPermissions: "rwm"},
		}
		hc.AutoRemove = true
		hc.RestartPolicy = docker.NeverRestart()
	})
	require.NoError(t, err, "Failed to start tailscale container")

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
		defer cancel()
		if dev, err := findDeviceByHostname(cleanupCtx, controller, clientHostname); err == nil && dev != nil {
			_ = controller.DeleteDevice(cleanupCtx, dev)
		}
		_ = pool.Purge(resource)
	})

	// Wait for tailscaled to be ready, then join the tailnet.
	require.Eventually(t, func() bool {
		exitCode, _ := resource.Exec([]string{"tailscale", "status"}, dockertest.ExecOptions{})
		return exitCode == 0 || exitCode == 1 // 1 = "not logged in" means daemon is running
	}, 30*time.Second, 1*time.Second, "tailscaled did not become ready")

	var stdout, stderr bytes.Buffer
	exitCode, err := resource.Exec(
		append([]string{"tailscale", "up"}, tsArgs...),
		dockertest.ExecOptions{StdOut: &stdout, StdErr: &stderr},
	)
	require.NoError(t, err, "Failed to exec tailscale up")
	require.Equal(t, 0, exitCode, "tailscale up failed: stdout=%s stderr=%s", stdout.String(), stderr.String())

	require.Eventually(t, func() bool {
		var out bytes.Buffer
		code, _ := resource.Exec([]string{"tailscale", "status"}, dockertest.ExecOptions{StdOut: &out})
		return code == 0
	}, 1*time.Minute, 2*time.Second, "Test client did not come online")

	t.Logf("Test client container started and joined tailnet")
	return resource
}

// dockerFetchURL runs wget inside the Docker container and returns the trimmed output.
// Returns empty string on failure. Uses timeout wrapper to prevent hanging on DNS
// resolution when exit node routing is set but not yet functional.
func dockerFetchURL(t *testing.T, resource *dockertest.Resource, url string) string {
	t.Helper()
	var stdout bytes.Buffer
	exitCode, err := resource.Exec(
		[]string{"timeout", "15", "wget", "-qO-", "-T", "10", url},
		dockertest.ExecOptions{StdOut: &stdout},
	)
	if err != nil || exitCode != 0 {
		return ""
	}
	return strings.TrimSpace(stdout.String())
}

// getTestRegion returns the test region for a provider, checking env var override first.
func getTestRegion(providerName string) string {
	if envVar, ok := regionEnvVars[providerName]; ok {
		if region := os.Getenv(envVar); region != "" {
			return region
		}
	}
	if region, ok := defaultTestRegions[providerName]; ok {
		return region
	}
	return ""
}

// loadRealConfig loads configuration for real integration testing.
func loadRealConfig(t *testing.T) (*config.Config, error) {
	t.Helper()
	if configPath := os.Getenv("TSCLOUDVPN_CONFIG"); configPath != "" {
		return config.LoadConfig(configPath)
	}
	return config.LoadFromEnv()
}

// hasControlAPIConfigured checks if Tailscale or Headscale is configured.
func hasControlAPIConfigured(cfg *config.Config) bool {
	return (cfg.Control.Tailscale.ClientID != "" && cfg.Control.Tailscale.ClientSecret != "") ||
		(cfg.Control.Headscale.APIKey != "")
}

// waitForDeviceRegistration polls the control API until a device with the
// expected hostname appears, or the timeout expires.
func waitForDeviceRegistration(ctx context.Context, controller controlapi.ControlApi, expectedHostname string, timeout time.Duration) (*controlapi.Device, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("timeout waiting for device %s to register", expectedHostname)
		case <-ticker.C:
			devices, err := controller.ListDevices(ctx)
			if err != nil {
				continue
			}
			for _, device := range devices {
				if device.Hostname == expectedHostname {
					return &device, nil
				}
			}
		}
	}
}

// findDeviceByHostname searches for a device with the given hostname.
func findDeviceByHostname(ctx context.Context, controller controlapi.ControlApi, hostname string) (*controlapi.Device, error) {
	devices, err := controller.ListDevices(ctx)
	if err != nil {
		return nil, err
	}
	for _, device := range devices {
		if device.Hostname == hostname {
			return &device, nil
		}
	}
	return nil, fmt.Errorf("device with hostname %s not found", hostname)
}

// generateTestSSHKey creates an ephemeral ed25519 SSH keypair for the test.
// Returns the signer (for SSH client auth) and the public key in authorized_keys format.
func generateTestSSHKey(t *testing.T) (ssh.Signer, string) {
	t.Helper()
	_, privKey, err := ed25519.GenerateKey(crypto_rand.Reader)
	require.NoError(t, err, "Failed to generate SSH key")

	signer, err := ssh.NewSignerFromKey(privKey)
	require.NoError(t, err, "Failed to create SSH signer")

	pubKey := ssh.MarshalAuthorizedKey(signer.PublicKey())
	return signer, strings.TrimSpace(string(pubKey))
}

// sshTailCloudInitLog resolves the VM's public IP (retrying until available),
// connects via SSH (retrying until reachable), and streams
// cloud-init-output.log via tail -f. Blocks until ctx is cancelled (which
// closes the connection and unblocks the stream).
// Intended to be called in a goroutine.
func sshTailCloudInitLog(ctx context.Context, t *testing.T, getter providers.PublicIPGetter, instance providers.Instance, signer ssh.Signer) {
	sshCfg := &ssh.ClientConfig{
		User:            getter.DebugSSHUser(),
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}

	// Wait for the public IP to be assigned.
	var addr string
	for {
		ip, err := getter.GetPublicIP(ctx, instance)
		if err == nil {
			addr = net.JoinHostPort(ip.String(), "22")
			t.Logf("Public IP for %s: %s", instance.Hostname, ip)
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}

	// Retry SSH connect + handshake until the VM accepts connections.
	var client *ssh.Client
	var conn net.Conn
	for {
		dialConn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		if err == nil {
			sshConn, chans, reqs, sshErr := ssh.NewClientConn(dialConn, addr, sshCfg)
			if sshErr == nil {
				client = ssh.NewClient(sshConn, chans, reqs)
				conn = dialConn
				t.Logf("SSH connected to %s", addr)
				break
			}
			dialConn.Close()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}

	session, err := client.NewSession()
	if err != nil {
		client.Close()
		t.Logf("SSH session to %s failed: %v", addr, err)
		return
	}

	session.Stdout = t.Output()
	if err := session.Start("tail -f /var/log/cloud-init-output.log"); err != nil {
		session.Close()
		client.Close()
		t.Logf("SSH tail on %s failed: %v", addr, err)
		return
	}

	t.Logf("Streaming cloud-init-output.log from %s...", addr)

	// When ctx is cancelled, close the connection to unblock Wait.
	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	session.Wait()
	session.Close()
	client.Close()
	t.Logf("cloud-init log stream from %s ended", addr)
}
