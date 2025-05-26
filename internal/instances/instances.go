package instances

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/anupcshan/tscloudvpn/internal/controlapi"
	"github.com/anupcshan/tscloudvpn/internal/providers"
)

const (
	// Time from issuing CreateInstance to when the instance should be available on GetInstanceStatus.
	// This is needed to work around providers that have eventually consistent APIs (like Vultr).
	launchDetectTimeout = time.Minute
)

// Creator handles instance creation and setup
type Creator struct{}

// NewCreator creates a new instance creator
func NewCreator() *Creator {
	return &Creator{}
}

// Create creates and configures a new instance in the specified region
func (c *Creator) Create(
	ctx context.Context,
	logger *log.Logger,
	controller controlapi.ControlApi,
	provider providers.Provider,
	region string,
) error {
	authKey, err := controller.CreateKey(ctx)
	if err != nil {
		return err
	}

	launchTime := time.Now()

	createdInstance, err := provider.CreateInstance(ctx, region, authKey)
	if err != nil {
		logger.Printf("Failed to launch instance %s: %s", provider.Hostname(region), err)
		return err
	}

	hostname := createdInstance.Hostname
	logger.Printf("Launched instance %s", hostname)
	logger.Printf("Waiting for instance to be listed via provider API")

	// Wait for instance to be available in provider API
	if err := c.waitForInstanceInAPI(ctx, provider, region, hostname); err != nil {
		return err
	}

	logger.Printf("Instance available in provider API")
	logger.Printf("Waiting for instance to register on Tailscale")

	// Wait for instance to register and approve it as exit node
	if err := c.waitForTailscaleRegistration(ctx, logger, controller, provider, region, hostname, launchTime); err != nil {
		return err
	}

	return nil
}

// waitForInstanceInAPI waits for the instance to be available in the provider's API
func (c *Creator) waitForInstanceInAPI(
	ctx context.Context,
	provider providers.Provider,
	region string,
	hostname string,
) error {
	startTime := time.Now()
	for {
		if time.Since(startTime) > launchDetectTimeout {
			return fmt.Errorf("Instance %s failed to be available in provider API in %s", hostname, launchDetectTimeout)
		}

		status, err := provider.GetInstanceStatus(ctx, region)
		if err != nil {
			return err
		}

		if status != providers.InstanceStatusRunning {
			time.Sleep(time.Second)
			continue
		}

		break
	}
	return nil
}

// waitForTailscaleRegistration waits for the instance to register with Tailscale and approves it as an exit node
func (c *Creator) waitForTailscaleRegistration(
	ctx context.Context,
	logger *log.Logger,
	controller controlapi.ControlApi,
	provider providers.Provider,
	region string,
	hostname string,
	launchTime time.Time,
) error {
	for {
		time.Sleep(time.Second)

		status, err := provider.GetInstanceStatus(ctx, region)
		if err != nil {
			return err
		}

		if status != providers.InstanceStatusRunning {
			logger.Printf("Instance %s failed to launch", hostname)
			return fmt.Errorf("Instance no longer running")
		}

		devices, err := controller.ListDevices(ctx)
		if err != nil {
			return err
		}

		var deviceId string
		var nodeName string
		for _, device := range devices {
			if device.Hostname == hostname && launchTime.Before(device.Created) {
				deviceId = device.ID
				nodeName = strings.SplitN(device.Name, ".", 2)[0]
				logger.Printf("Instance registered on Tailscale with ID %s, name %s", deviceId, nodeName)
				break
			}
		}

		if deviceId == "" {
			continue
		}

		logger.Printf("Approving exit node %s", nodeName)
		if err := controller.ApproveExitNode(ctx, deviceId); err != nil {
			return err
		}

		break
	}

	return nil
}
