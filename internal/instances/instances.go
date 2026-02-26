package instances

import (
	"context"
	"log"

	"github.com/anupcshan/tscloudvpn/internal/controlapi"
	"github.com/anupcshan/tscloudvpn/internal/providers"
)

// Creator handles instance creation and setup
type Creator struct{}

// NewCreator creates a new instance creator
func NewCreator() *Creator {
	return &Creator{}
}

// Create creates a new instance in the specified region. It obtains an auth key
// and launches the cloud instance. The health check (monitorHealth) handles
// discovering the peer in Tailscale and transitioning the controller to Running.
// Exit node route approval is handled by Tailscale's autoApprovers ACL policy.
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

	createdInstance, err := provider.CreateInstance(ctx, region, authKey)
	if err != nil {
		logger.Printf("Failed to launch instance %s: %s", provider.Hostname(region), err)
		return providers.Instance{}, err
	}

	logger.Printf("Launched instance %s", createdInstance.Hostname)
	return createdInstance, nil
}
