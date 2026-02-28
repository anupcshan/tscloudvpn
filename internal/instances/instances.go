package instances

import (
	"context"
	"log"

	"github.com/anupcshan/tscloudvpn/internal/controlapi"
	"github.com/anupcshan/tscloudvpn/internal/providers"
)

// Creator handles instance creation and setup
type Creator struct {
	sshKey string
}

// NewCreator creates a new instance creator
func NewCreator(sshKey string) *Creator {
	return &Creator{sshKey: sshKey}
}

// Create creates a new instance in the specified region. It obtains an auth key,
// renders the init script, and launches the cloud instance. The health check
// (monitorHealth) handles discovering the peer in Tailscale and transitioning
// the controller to Running. Exit node route approval is handled by Tailscale's
// autoApprovers ACL policy.
func (c *Creator) Create(
	ctx context.Context,
	logger *log.Logger,
	controller controlapi.ControlApi,
	provider providers.Provider,
	region string,
) (providers.Instance, error) {
	authKey, err := controller.CreateKey(ctx)
	if err != nil {
		return providers.Instance{}, err
	}

	hostname := string(provider.Hostname(region))
	userData, err := providers.RenderUserData(hostname, authKey, c.sshKey)
	if err != nil {
		return providers.Instance{}, err
	}

	createdInstance, err := provider.CreateInstance(ctx, providers.CreateRequest{Region: region, UserData: userData, SSHKey: c.sshKey})
	if err != nil {
		logger.Printf("Failed to launch instance %s: %s", provider.Hostname(region), err)
		return providers.Instance{}, err
	}

	logger.Printf("Launched instance %s", createdInstance.Hostname)
	return createdInstance, nil
}
