package azure

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armsubscriptions"

	"github.com/anupcshan/tscloudvpn/internal/config"
	"github.com/anupcshan/tscloudvpn/internal/controlapi"
	"github.com/anupcshan/tscloudvpn/internal/providers"
)

const (
	providerName  = "azure"
	vmSize        = "Standard_B1s" // Cheapest general purpose VM
	publisher     = "Debian"
	offer         = "debian-12"
	sku           = "12-gen2"
	version       = "latest"
	cacheDuration = 24 * time.Hour // Cache regions and pricing for 24 hours
)

type regionInfo struct {
	Code        string
	LongName    string
	HourlyPrice float64
}

type azureProvider struct {
	subscriptionID string
	resourceGroup  string
	sshKey         string
	tenantID       string
	clientID       string
	clientSecret   string
	ownerID        string // Unique identifier for this tscloudvpn instance
	computeClient  *armcompute.VirtualMachinesClient
	networkClient  *armnetwork.VirtualNetworksClient
	pipClient      *armnetwork.PublicIPAddressesClient
	nicClient      *armnetwork.InterfacesClient
	subsClient     *armsubscriptions.Client

	// Cache for regions and pricing
	regionCacheLock sync.RWMutex
	regionCache     map[string]regionInfo
	regionCacheTime time.Time
}

func NewProvider(ctx context.Context, cfg *config.Config) (providers.Provider, error) {
	if cfg.Providers.Azure.SubscriptionID == "" {
		// No subscription ID set. Nothing to do
		return nil, nil
	}

	// Create credential from config
	cred, err := azidentity.NewClientSecretCredential(
		cfg.Providers.Azure.TenantID,
		cfg.Providers.Azure.ClientID,
		cfg.Providers.Azure.ClientSecret,
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create Azure credentials: %w", err)
	}

	// Create clients
	computeClient, err := armcompute.NewVirtualMachinesClient(cfg.Providers.Azure.SubscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create compute client: %w", err)
	}

	networkClient, err := armnetwork.NewVirtualNetworksClient(cfg.Providers.Azure.SubscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create network client: %w", err)
	}

	pipClient, err := armnetwork.NewPublicIPAddressesClient(cfg.Providers.Azure.SubscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create public IP client: %w", err)
	}

	nicClient, err := armnetwork.NewInterfacesClient(cfg.Providers.Azure.SubscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create NIC client: %w", err)
	}

	subsClient, err := armsubscriptions.NewClient(cred, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create subscriptions client: %w", err)
	}

	return &azureProvider{
		subscriptionID: cfg.Providers.Azure.SubscriptionID,
		resourceGroup:  cfg.Providers.Azure.ResourceGroup,
		tenantID:       cfg.Providers.Azure.TenantID,
		clientID:       cfg.Providers.Azure.ClientID,
		clientSecret:   cfg.Providers.Azure.ClientSecret,
		ownerID:        providers.GetOwnerID(cfg),
		sshKey:         cfg.SSH.PublicKey,
		computeClient:  computeClient,
		networkClient:  networkClient,
		pipClient:      pipClient,
		nicClient:      nicClient,
		subsClient:     subsClient,
	}, nil
}

func azureInstanceHostname(region string) string {
	return fmt.Sprintf("azure-%s", region)
}

func (a *azureProvider) CreateInstance(ctx context.Context, region string, key *controlapi.PreauthKey) (providers.InstanceID, error) {
	hostname := azureInstanceHostname(region)
	vmName := fmt.Sprintf("tscloudvpn-%s", region)

	// Prepare cloud-init data
	tmplOut := new(bytes.Buffer)
	if err := template.Must(template.New("tmpl").Parse(providers.InitData)).Execute(tmplOut, struct {
		Args   string
		OnExit string
		SSHKey string
	}{
		Args: fmt.Sprintf(
			`%s --hostname=%s`,
			strings.Join(key.GetCLIArgs(), " "),
			hostname,
		),
		OnExit: a.generateOnExitScript(),
		SSHKey: a.sshKey,
	}); err != nil {
		return providers.InstanceID{}, fmt.Errorf("failed to generate cloud-init: %w", err)
	}

	// Base64 encode the cloud-init data
	cloudInitData := base64.StdEncoding.EncodeToString(tmplOut.Bytes())

	// First, ensure we have the necessary network resources
	if err := a.ensureNetworkResources(ctx, region, vmName); err != nil {
		return providers.InstanceID{}, fmt.Errorf("failed to create network resources: %w", err)
	}

	// Create VM
	vmParams := armcompute.VirtualMachine{
		Location: to.Ptr(region),
		Properties: &armcompute.VirtualMachineProperties{
			HardwareProfile: &armcompute.HardwareProfile{
				VMSize: to.Ptr(armcompute.VirtualMachineSizeTypes(vmSize)),
			},
			StorageProfile: &armcompute.StorageProfile{
				ImageReference: &armcompute.ImageReference{
					Publisher: to.Ptr(publisher),
					Offer:     to.Ptr(offer),
					SKU:       to.Ptr(sku),
					Version:   to.Ptr(version),
				},
				OSDisk: &armcompute.OSDisk{
					CreateOption: to.Ptr(armcompute.DiskCreateOptionTypesFromImage),
					DeleteOption: to.Ptr(armcompute.DiskDeleteOptionTypesDelete),
					ManagedDisk: &armcompute.ManagedDiskParameters{
						StorageAccountType: to.Ptr(armcompute.StorageAccountTypesStandardLRS),
					},
				},
			},
			OSProfile: &armcompute.OSProfile{
				ComputerName:  to.Ptr(vmName),
				AdminUsername: to.Ptr("azureuser"),
				LinuxConfiguration: &armcompute.LinuxConfiguration{
					DisablePasswordAuthentication: to.Ptr(true),
					SSH: &armcompute.SSHConfiguration{
						PublicKeys: []*armcompute.SSHPublicKey{
							{
								Path:    to.Ptr("/home/azureuser/.ssh/authorized_keys"),
								KeyData: to.Ptr(a.sshKey),
							},
						},
					},
				},
				CustomData: to.Ptr(cloudInitData),
			},
			NetworkProfile: &armcompute.NetworkProfile{
				NetworkInterfaces: []*armcompute.NetworkInterfaceReference{
					{
						ID: to.Ptr(fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/networkInterfaces/%s-nic", a.subscriptionID, a.resourceGroup, vmName)),
						Properties: &armcompute.NetworkInterfaceReferenceProperties{
							Primary:      to.Ptr(true),
							DeleteOption: to.Ptr(armcompute.DeleteOptionsDelete),
						},
					},
				},
			},
		},
		Tags: map[string]*string{
			"created-by":          to.Ptr("tscloudvpn"),
			"region":              to.Ptr(region),
			providers.OwnerTagKey: to.Ptr(a.ownerID),
		},
	}

	// Create the VM
	poller, err := a.computeClient.BeginCreateOrUpdate(ctx, a.resourceGroup, vmName, vmParams, nil)
	if err != nil {
		return providers.InstanceID{}, fmt.Errorf("failed to create VM: %w", err)
	}

	// Wait for completion
	_, err = poller.PollUntilDone(ctx, nil)
	if err != nil {
		return providers.InstanceID{}, fmt.Errorf("failed to complete VM creation: %w", err)
	}

	log.Printf("Created Azure VM %s in region %s", vmName, region)

	return providers.InstanceID{
		Hostname:     hostname,
		ProviderID:   vmName,
		ProviderName: providerName,
	}, nil
}

func (a *azureProvider) ensureNetworkResources(ctx context.Context, region, vmName string) error {
	vnetName := fmt.Sprintf("%s-vnet", vmName)
	subnetName := fmt.Sprintf("%s-subnet", vmName)
	pipName := fmt.Sprintf("%s-pip", vmName)
	nicName := fmt.Sprintf("%s-nic", vmName)

	// Create Public IP
	pipParams := armnetwork.PublicIPAddress{
		Location: to.Ptr(region),
		Properties: &armnetwork.PublicIPAddressPropertiesFormat{
			PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
		},
	}

	pipPoller, err := a.pipClient.BeginCreateOrUpdate(ctx, a.resourceGroup, pipName, pipParams, nil)
	if err != nil {
		return fmt.Errorf("failed to create public IP: %w", err)
	}

	// Create Virtual Network with subnet
	vnetParams := armnetwork.VirtualNetwork{
		Location: to.Ptr(region),
		Properties: &armnetwork.VirtualNetworkPropertiesFormat{
			AddressSpace: &armnetwork.AddressSpace{
				AddressPrefixes: []*string{to.Ptr("10.0.0.0/16")},
			},
			Subnets: []*armnetwork.Subnet{
				{
					Name: to.Ptr(subnetName),
					Properties: &armnetwork.SubnetPropertiesFormat{
						AddressPrefix: to.Ptr("10.0.0.0/24"),
					},
				},
			},
		},
	}

	vnetPoller, err := a.networkClient.BeginCreateOrUpdate(ctx, a.resourceGroup, vnetName, vnetParams, nil)
	if err != nil {
		return fmt.Errorf("failed to create VNet: %w", err)
	}

	// Wait for resources to be created
	pipResult, err := pipPoller.PollUntilDone(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to complete public IP creation: %w", err)
	}

	vnetResult, err := vnetPoller.PollUntilDone(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to complete VNet creation: %w", err)
	}

	// Create Network Interface
	nicParams := armnetwork.Interface{
		Location: to.Ptr(region),
		Properties: &armnetwork.InterfacePropertiesFormat{
			IPConfigurations: []*armnetwork.InterfaceIPConfiguration{
				{
					Name: to.Ptr("ipconfig1"),
					Properties: &armnetwork.InterfaceIPConfigurationPropertiesFormat{
						PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
						PublicIPAddress: &armnetwork.PublicIPAddress{
							ID: pipResult.ID,
							Properties: &armnetwork.PublicIPAddressPropertiesFormat{
								DeleteOption: to.Ptr(armnetwork.DeleteOptionsDelete),
							},
						},
						Subnet: &armnetwork.Subnet{
							ID: vnetResult.Properties.Subnets[0].ID,
						},
					},
				},
			},
		},
	}

	nicPoller, err := a.nicClient.BeginCreateOrUpdate(ctx, a.resourceGroup, nicName, nicParams, nil)
	if err != nil {
		return fmt.Errorf("failed to create NIC: %w", err)
	}

	_, err = nicPoller.PollUntilDone(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to complete NIC creation: %w", err)
	}

	return nil
}

func (a *azureProvider) DeleteInstance(ctx context.Context, instanceID providers.InstanceID) error {
	vmName := instanceID.ProviderID

	// Delete VM
	poller, err := a.computeClient.BeginDelete(ctx, a.resourceGroup, vmName, nil)
	if err != nil {
		return fmt.Errorf("failed to delete VM: %w", err)
	}

	_, err = poller.PollUntilDone(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to complete VM deletion: %w", err)
	}

	log.Printf("Deleted Azure VM %s", vmName)
	return nil
}

func (a *azureProvider) GetInstanceStatus(ctx context.Context, region string) (providers.InstanceStatus, error) {
	vmName := fmt.Sprintf("tscloudvpn-%s", region)

	_, err := a.computeClient.Get(ctx, a.resourceGroup, vmName, nil)
	if err != nil {
		var responseError *azcore.ResponseError
		if errors.As(err, &responseError) && responseError.StatusCode == 404 {
			return providers.InstanceStatusMissing, nil
		}
		return providers.InstanceStatusMissing, err
	}

	return providers.InstanceStatusRunning, nil
}

func (a *azureProvider) ListInstances(ctx context.Context, region string) ([]providers.InstanceID, error) {
	vmName := fmt.Sprintf("tscloudvpn-%s", region)

	vm, err := a.computeClient.Get(ctx, a.resourceGroup, vmName, nil)
	if err != nil {
		var responseError *azcore.ResponseError
		if errors.As(err, &responseError) && responseError.StatusCode == 404 {
			return []providers.InstanceID{}, nil
		}
		return nil, err
	}

	// Verify this VM belongs to us by checking the owner tag
	if vm.Tags != nil {
		if owner, ok := vm.Tags[providers.OwnerTagKey]; ok && owner != nil && *owner != a.ownerID {
			// VM exists but belongs to different owner
			return []providers.InstanceID{}, nil
		}
	}

	var createdAt time.Time
	if vm.Properties != nil && vm.Properties.TimeCreated != nil {
		createdAt = *vm.Properties.TimeCreated
	}

	return []providers.InstanceID{
		{
			Hostname:     azureInstanceHostname(region),
			ProviderID:   vmName,
			ProviderName: providerName,
			CreatedAt:    createdAt,
		},
	}, nil
}

func (a *azureProvider) ListRegions(ctx context.Context) ([]providers.Region, error) {
	if a.subsClient == nil {
		return nil, fmt.Errorf("azure subscriptions client not initialized")
	}

	regions, err := a.loadRegions(ctx)
	if err != nil {
		return nil, err
	}

	var result []providers.Region
	for _, region := range regions {
		result = append(result, providers.Region{
			Code:     region.Code,
			LongName: region.LongName,
		})
	}

	return result, nil
}

func (a *azureProvider) Hostname(region string) providers.HostName {
	return providers.HostName(azureInstanceHostname(region))
}

func (a *azureProvider) GetRegionPrice(region string) float64 {
	// If no Azure client available, use estimation
	if a.subsClient == nil {
		return a.estimateRegionPrice(region)
	}

	regions, err := a.loadRegions(context.Background())
	if err != nil {
		log.Printf("Failed to load region pricing: %v", err)
		return a.estimateRegionPrice(region) // Fallback to estimation
	}

	if regionInfo, ok := regions[region]; ok {
		return regionInfo.HourlyPrice
	}
	return a.estimateRegionPrice(region) // Fallback to estimation
}

func (a *azureProvider) loadRegions(ctx context.Context) (map[string]regionInfo, error) {
	if a.subsClient == nil {
		return nil, fmt.Errorf("azure subscriptions client not initialized")
	}

	a.regionCacheLock.Lock()
	defer a.regionCacheLock.Unlock()

	// Check cache first
	if time.Since(a.regionCacheTime) < cacheDuration && a.regionCache != nil {
		return a.regionCache, nil
	}

	// Fetch locations from Azure API
	locations, err := a.fetchAzureLocations(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch Azure locations: %w", err)
	}

	// Build region cache with pricing estimates
	regionCache := make(map[string]regionInfo)
	for _, location := range locations {
		regionCache[location.Code] = regionInfo{
			Code:        location.Code,
			LongName:    location.LongName,
			HourlyPrice: a.estimateRegionPrice(location.Code),
		}
	}

	a.regionCache = regionCache
	a.regionCacheTime = time.Now()

	return regionCache, nil
}

func (a *azureProvider) fetchAzureLocations(ctx context.Context) ([]providers.Region, error) {
	pager := a.subsClient.NewListLocationsPager(a.subscriptionID, nil)

	var locations []providers.Region
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to get locations page: %w", err)
		}

		for _, location := range page.Value {
			if location.Name == nil || location.DisplayName == nil {
				continue
			}

			// Filter for regions that support virtual machines
			if location.Metadata != nil && location.Metadata.RegionType != nil &&
				*location.Metadata.RegionType == "Physical" {
				locations = append(locations, providers.Region{
					Code:     *location.Name,
					LongName: *location.DisplayName,
				})
			}
		}
	}

	// Sort by region code for consistent ordering
	sort.Slice(locations, func(i, j int) bool {
		return locations[i].Code < locations[j].Code
	})

	return locations, nil
}

func (a *azureProvider) generateOnExitScript() string {
	script := `#!/bin/bash
set -e

# Get Azure instance metadata
IMDS_URL="http://169.254.169.254/metadata/instance?api-version=2021-02-01"
METADATA=$(curl -s -H "Metadata: true" "$IMDS_URL")
VM_NAME=$(echo "$METADATA" | jq -r '.compute.name')
RESOURCE_GROUP=$(echo "$METADATA" | jq -r '.compute.resourceGroupName')
SUBSCRIPTION_ID=$(echo "$METADATA" | jq -r '.compute.subscriptionId')

# Get access token for Azure API
TOKEN_URL="https://login.microsoftonline.com/TENANT_ID/oauth2/v2.0/token"
TOKEN_RESPONSE=$(curl -s -X POST "$TOKEN_URL" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "grant_type=client_credentials" \
  -d "client_id=CLIENT_ID" \
  -d "client_secret=CLIENT_SECRET" \
  -d "scope=https://management.azure.com/.default")
ACCESS_TOKEN=$(echo "$TOKEN_RESPONSE" | jq -r '.access_token')

if [ "$ACCESS_TOKEN" = "null" ] || [ -z "$ACCESS_TOKEN" ]; then
  echo "Failed to get access token, falling back to poweroff"
  sudo /sbin/poweroff
  exit 0
fi

# Delete VM (this automatically deletes OS disk, NIC, and Public IP)
echo "Deleting VM: $VM_NAME (will cascade to associated resources)"
AZURE_API="https://management.azure.com/subscriptions/$SUBSCRIPTION_ID/resourceGroups/$RESOURCE_GROUP"
curl -s -X DELETE "$AZURE_API/providers/Microsoft.Compute/virtualMachines/$VM_NAME?api-version=2023-03-01" \
  -H "Authorization: Bearer $ACCESS_TOKEN" \
  -H "Content-Type: application/json" || {
  echo "Failed to delete VM, falling back to poweroff"
  sudo /sbin/poweroff
  exit 0
}

echo "VM deletion initiated - OS disk, NIC, and Public IP will be automatically cleaned up"
`
	// Replace placeholders with actual values
	script = strings.ReplaceAll(script, "TENANT_ID", a.tenantID)
	script = strings.ReplaceAll(script, "CLIENT_ID", a.clientID)
	script = strings.ReplaceAll(script, "CLIENT_SECRET", a.clientSecret)
	return script
}

func (a *azureProvider) estimateRegionPrice(region string) float64 {
	// Provide rough pricing estimates for Standard_B1s based on common Azure pricing patterns
	// These are approximations - in production you might want to use Azure's Rate Card API
	// or Azure Pricing Calculator API for exact pricing

	// US regions tend to be cheapest
	if strings.Contains(region, "us") {
		return 0.0104
	}

	// European regions
	if strings.Contains(region, "europe") || strings.Contains(region, "uk") ||
		strings.Contains(region, "france") || strings.Contains(region, "germany") ||
		strings.Contains(region, "switzerland") || strings.Contains(region, "norway") {
		return 0.0125
	}

	// Asia Pacific regions
	if strings.Contains(region, "asia") || strings.Contains(region, "australia") ||
		strings.Contains(region, "japan") || strings.Contains(region, "korea") ||
		strings.Contains(region, "india") {
		return 0.0135
	}

	// Canada regions
	if strings.Contains(region, "canada") {
		return 0.0114
	}

	// Brazil and other South America
	if strings.Contains(region, "brazil") || strings.Contains(region, "south") {
		return 0.0156
	}

	// Default fallback
	return 0.0125
}

func init() {
	providers.Register(providerName, NewProvider)
}
