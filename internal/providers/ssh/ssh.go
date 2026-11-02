// Package ssh runs service instances as Docker containers on SSH hosts.
package ssh

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/anupcshan/tscloudvpn/internal/config"
	"github.com/anupcshan/tscloudvpn/internal/providers"
	"golang.org/x/crypto/ssh"
)

const (
	defaultPort          = 22
	defaultUser          = "root"
	defaultDockerCommand = "docker"
	defaultImage         = "ubuntu:24.04"

	// bootstrapScript installs the tools the service init scripts assume are
	// present in a cloud image. The Docker Ubuntu image is a minimal rootfs.
	bootstrapScript = `set -eux
export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y --no-install-recommends curl ca-certificates sudo python3 iproute2
`
)

func init() {
	providers.Register("ssh", "SSH", New)
}

type host struct {
	runner        *sshRunner
	dockerCommand []string
}

// Provider runs service instances as Docker containers on SSH hosts.
type Provider struct {
	hosts   map[string]*host
	ownerID string
}

// New creates an SSH provider from the configured hosts.
func New(_ context.Context, cfg *config.Config) (providers.Provider, error) {
	if len(cfg.Providers.SSH.Hosts) == 0 {
		return nil, nil
	}

	p := &Provider{
		hosts:   make(map[string]*host),
		ownerID: providers.GetOwnerID(cfg),
	}

	for name, hc := range cfg.Providers.SSH.Hosts {
		if name == "" {
			return nil, errors.New("ssh host name is required")
		}
		h, err := newHost(hc)
		if err != nil {
			return nil, fmt.Errorf("ssh host %q: %w", name, err)
		}
		p.hosts[name] = h
	}

	return p, nil
}

func newHost(hc config.SSHHost) (*host, error) {
	if hc.Address == "" {
		return nil, errors.New("address is required")
	}
	if hc.PrivateKeyFile == "" {
		return nil, errors.New("private_key_file is required")
	}
	if hc.HostKey == "" {
		return nil, errors.New("host_key is required")
	}

	keyBytes, err := os.ReadFile(hc.PrivateKeyFile)
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}

	hostKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(hc.HostKey))
	if err != nil {
		return nil, fmt.Errorf("parse host_key: %w", err)
	}

	user := hc.User
	if user == "" {
		user = defaultUser
	}
	port := hc.Port
	if port == 0 {
		port = defaultPort
	}
	dockerCommand := hc.DockerCommand
	if dockerCommand == "" {
		dockerCommand = defaultDockerCommand
	}

	return &host{
		runner: &sshRunner{
			addr: net.JoinHostPort(hc.Address, strconv.Itoa(port)),
			config: &ssh.ClientConfig{
				User:              user,
				Auth:              []ssh.AuthMethod{ssh.PublicKeys(signer)},
				HostKeyCallback:   ssh.FixedHostKey(hostKey),
				HostKeyAlgorithms: hostKeyAlgorithms(hostKey),
				Timeout:           30 * time.Second,
			},
		},
		dockerCommand: strings.Fields(dockerCommand),
	}, nil
}

func hostKeyAlgorithms(key ssh.PublicKey) []string {
	if key.Type() == ssh.KeyAlgoRSA {
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA}
	}
	return []string{key.Type()}
}

type sshRunner struct {
	addr   string
	config *ssh.ClientConfig
}

func (r *sshRunner) Run(command string) ([]byte, error) {
	client, err := ssh.Dial("tcp", r.addr, r.config)
	if err != nil {
		return nil, err
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	defer session.Close()

	var stderr strings.Builder
	session.Stderr = &stderr

	out, err := session.Output(command)
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return out, fmt.Errorf("%w: %s", err, msg)
		}
		return out, err
	}
	return out, nil
}

// CreateInstance starts a container and returns its Docker ID.
func (p *Provider) CreateInstance(_ context.Context, req providers.CreateRequest) (providers.Instance, error) {
	h, ok := p.hosts[req.Region]
	if !ok {
		return providers.Instance{}, fmt.Errorf("unknown ssh host %q", req.Region)
	}

	service := req.Tags[providers.ServiceTagKey]
	if service == "" {
		return providers.Instance{}, errors.New("missing service tag")
	}

	script := bootstrapScript + req.UserData
	encoded := base64.StdEncoding.EncodeToString([]byte(script))

	args := h.dockerArgs(
		"run", "-d", "--rm",
		"--label", "tscloudvpn-owner="+p.ownerID,
		"--label", "tscloudvpn-hostname="+req.Hostname,
		"--label", "tscloudvpn-region="+req.Region,
	)
	args = append(args, permissions(service)...)
	args = append(args, defaultImage, "bash", "-c", "echo "+encoded+" | base64 -d | bash")

	out, err := h.runner.Run(joinCommand(args))
	if err != nil {
		return providers.Instance{}, fmt.Errorf("start container on %s: %w", req.Region, err)
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return providers.Instance{}, fmt.Errorf("docker run on %s returned no container id", req.Region)
	}

	return providers.Instance{
		Hostname:     req.Hostname,
		ProviderID:   id,
		ProviderName: "ssh",
		Region:       req.Region,
		CreatedAt:    time.Now(),
	}, nil
}

// DeleteInstance removes the container. It does not touch the host.
func (p *Provider) DeleteInstance(_ context.Context, instance providers.Instance) error {
	h, ok := p.hosts[instance.Region]
	if !ok {
		return fmt.Errorf("unknown ssh host %q", instance.Region)
	}
	if instance.ProviderID == "" {
		return errors.New("missing container id")
	}

	_, err := h.runner.Run(joinCommand(h.dockerArgs("rm", "-f", instance.ProviderID)))
	if err != nil && strings.Contains(err.Error(), "No such container") {
		return nil
	}
	return err
}

// ListInstances returns the containers running on a host.
func (p *Provider) ListInstances(_ context.Context, region string) ([]providers.Instance, error) {
	h, ok := p.hosts[region]
	if !ok {
		return nil, fmt.Errorf("unknown ssh host %q", region)
	}

	out, err := h.runner.Run(joinCommand(h.dockerArgs(
		"ps", "--quiet",
		"--filter", "label=tscloudvpn-owner="+p.ownerID,
		"--filter", "label=tscloudvpn-region="+region,
	)))
	if err != nil {
		return nil, fmt.Errorf("list containers on %s: %w", region, err)
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return nil, nil
	}

	inspectArgs := append([]string{"inspect", "--format", "{{json .}}"}, ids...)
	out, err = h.runner.Run(joinCommand(h.dockerArgs(inspectArgs...)))
	if err != nil {
		return nil, fmt.Errorf("inspect containers on %s: %w", region, err)
	}

	var instances []providers.Instance
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		var container dockerContainer
		if err := json.Unmarshal([]byte(line), &container); err != nil {
			return nil, fmt.Errorf("parse container on %s: %w", region, err)
		}
		hostname := container.Config.Labels["tscloudvpn-hostname"]
		if hostname == "" {
			continue
		}
		instances = append(instances, providers.Instance{
			Hostname:     hostname,
			ProviderID:   container.ID,
			ProviderName: "ssh",
			Region:       region,
			CreatedAt:    container.Created,
		})
	}
	return instances, nil
}

// GetInstanceStatus reports whether a container exists in a region.
func (p *Provider) GetInstanceStatus(ctx context.Context, region string) (providers.InstanceStatus, error) {
	instances, err := p.ListInstances(ctx, region)
	if err != nil {
		return providers.InstanceStatusMissing, err
	}
	if len(instances) > 0 {
		return providers.InstanceStatusRunning, nil
	}
	return providers.InstanceStatusMissing, nil
}

// ListRegions returns the configured hosts.
func (p *Provider) ListRegions(context.Context) ([]providers.Region, error) {
	regions := make([]providers.Region, 0, len(p.hosts))
	for name := range p.hosts {
		regions = append(regions, providers.Region{Code: name, LongName: name})
	}
	return regions, nil
}

// GetRegionHourlyEstimate returns zero because the host is already paid for.
func (p *Provider) GetRegionHourlyEstimate(string) float64 { return 0 }

func permissions(service string) []string {
	base := []string{
		"--cap-add=NET_ADMIN",
		"--cap-add=NET_RAW",
		"--device=/dev/net/tun",
	}
	switch service {
	case "exit":
		return append(base,
			"--sysctl=net.ipv4.ip_forward=1",
			"--sysctl=net.ipv6.conf.all.forwarding=1",
			"--publish=41641:41641/udp",
		)
	case "files":
		return append(base,
			"--cap-add=SYS_ADMIN",
			"--device=/dev/nbd0",
			"--device=/dev/zfs",
		)
	default:
		return base
	}
}

func (h *host) dockerArgs(args ...string) []string {
	out := make([]string, 0, len(h.dockerCommand)+len(args))
	out = append(out, h.dockerCommand...)
	return append(out, args...)
}

func joinCommand(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = shellQuote(arg)
	}
	return strings.Join(quoted, " ")
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

type dockerContainer struct {
	ID      string    `json:"Id"`
	Created time.Time `json:"Created"`
	Config  struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

var _ providers.Provider = (*Provider)(nil)
