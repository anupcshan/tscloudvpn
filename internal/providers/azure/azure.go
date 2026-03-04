package azure

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armsubscriptions"

	"github.com/anupcshan/tscloudvpn/internal/config"
	"github.com/anupcshan/tscloudvpn/internal/providers"
)

const (
	providerName  = "azure"
	publisher     = "Canonical"
	offer         = "ubuntu-24_04-lts"
	skuX64        = "server"
	skuArm64      = "server-arm64"
	version       = "latest"
	cacheDuration = 24 * time.Hour // Cache regions and pricing for 24 hours

	retailPricesURL = "https://prices.azure.com/api/retail/prices"
)

type regionInfo struct {
	Code        string
	LongName    string
	HourlyPrice float64
}

// regionVMType represents a VM size available in a region with its hourly cost.
type regionVMType struct {
	VMSize     string
	HourlyCost float64
	Arm64      bool
}

// vmSKUInfo holds metadata about a VM SKU from the Resource SKUs API.
type vmSKUInfo struct {
	Arm64 bool
}

// retailPricesResponse represents the Azure Retail Prices API response.
type retailPricesResponse struct {
	Items        []retailPriceItem `json:"Items"`
	NextPageLink string            `json:"NextPageLink"`
	Count        int               `json:"Count"`
}

// retailPriceItem represents a single pricing item from the API.
type retailPriceItem struct {
	ArmSkuName    string  `json:"armSkuName"`
	ArmRegionName string  `json:"armRegionName"`
	UnitPrice     float64 `json:"unitPrice"`
	MeterName     string  `json:"meterName"`
	ProductName   string  `json:"productName"`
	UnitOfMeasure string  `json:"unitOfMeasure"`
}

type azureProvider struct {
	subscriptionID string
	resourceGroup  string
	ownerID        string // Unique identifier for this tscloudvpn instance
	computeClient  *armcompute.VirtualMachinesClient
	networkClient  *armnetwork.VirtualNetworksClient
	pipClient      *armnetwork.PublicIPAddressesClient
	nicClient      *armnetwork.InterfacesClient
	subsClient     *armsubscriptions.Client
	skuClient      *armcompute.ResourceSKUsClient

	// Cache for regions and pricing
	regionCacheLock sync.RWMutex
	regionCache     map[string]regionInfo
	regionCacheTime time.Time

	// Cache for per-region VM types sorted by price (cheapest first)
	vmTypeCacheLock sync.RWMutex
	vmTypeCache     map[string][]regionVMType
	vmTypeCacheTime time.Time
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

	skuClient, err := armcompute.NewResourceSKUsClient(cfg.Providers.Azure.SubscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource SKUs client: %w", err)
	}

	prov := &azureProvider{
		subscriptionID: cfg.Providers.Azure.SubscriptionID,
		resourceGroup:  cfg.Providers.Azure.ResourceGroup,
		ownerID:        providers.GetOwnerID(cfg),
		computeClient:  computeClient,
		networkClient:  networkClient,
		pipClient:      pipClient,
		nicClient:      nicClient,
		subsClient:     subsClient,
		skuClient:      skuClient,
	}

	go prov.ensureVMTypeCache()

	return prov, nil
}

func azureInstanceHostname(region string) string {
	return fmt.Sprintf("azure-%s", region)
}

func (a *azureProvider) CreateInstance(ctx context.Context, req providers.CreateRequest) (providers.Instance, error) {
	if err := a.ensureVMTypeCache(); err != nil {
		return providers.Instance{}, fmt.Errorf("failed to load VM pricing: %w", err)
	}

	a.vmTypeCacheLock.RLock()
	vmTypes := a.vmTypeCache[req.Region]
	a.vmTypeCacheLock.RUnlock()

	if len(vmTypes) == 0 {
		return providers.Instance{}, fmt.Errorf("no VM types available for region %s", req.Region)
	}

	hostname := azureInstanceHostname(req.Region)
	vmName := fmt.Sprintf("tscloudvpn-%s", req.Region)

	// Base64 encode the cloud-init data
	cloudInitData := base64.StdEncoding.EncodeToString([]byte(req.UserData))

	// Create network resources once (before trying VM sizes)
	if err := a.ensureNetworkResources(ctx, req.Region, vmName); err != nil {
		return providers.Instance{}, fmt.Errorf("failed to create network resources: %w", err)
	}

	// Try VM types in order of price (cheapest first), falling back on errors
	var lastErr error
	for _, mt := range vmTypes {
		log.Printf("Creating Azure VM %s in %s using %s ($%.4f/hr)", vmName, req.Region, mt.VMSize, mt.HourlyCost)

		imageSKU := skuX64
		if mt.Arm64 {
			imageSKU = skuArm64
		}

		vmParams := armcompute.VirtualMachine{
			Location: to.Ptr(req.Region),
			Properties: &armcompute.VirtualMachineProperties{
				HardwareProfile: &armcompute.HardwareProfile{
					VMSize: to.Ptr(armcompute.VirtualMachineSizeTypes(mt.VMSize)),
				},
				StorageProfile: &armcompute.StorageProfile{
					ImageReference: &armcompute.ImageReference{
						Publisher: to.Ptr(publisher),
						Offer:     to.Ptr(offer),
						SKU:       to.Ptr(imageSKU),
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
									KeyData: to.Ptr(req.SSHKey),
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
				"region":              to.Ptr(req.Region),
				providers.OwnerTagKey: to.Ptr(a.ownerID),
			},
		}

		poller, err := a.computeClient.BeginCreateOrUpdate(ctx, a.resourceGroup, vmName, vmParams, nil)
		if err != nil {
			lastErr = fmt.Errorf("failed to create VM with %s: %w", mt.VMSize, err)
			log.Printf("%v, trying next VM size", lastErr)
			continue
		}

		_, err = poller.PollUntilDone(ctx, nil)
		if err != nil {
			lastErr = fmt.Errorf("failed to complete VM creation with %s: %w", mt.VMSize, err)
			log.Printf("%v, trying next VM size", lastErr)
			continue
		}

		log.Printf("Created Azure VM %s in region %s (size: %s)", vmName, req.Region, mt.VMSize)
		return providers.Instance{
			Hostname:     hostname,
			ProviderID:   vmName,
			ProviderName: providerName,
			HourlyCost:   mt.HourlyCost,
		}, nil
	}

	return providers.Instance{}, fmt.Errorf("all VM sizes exhausted for region %s: %w", req.Region, lastErr)
}

func (a *azureProvider) DebugSSHUser() string { return "azureuser" }

func (a *azureProvider) GetPublicIP(ctx context.Context, instance providers.Instance) (netip.Addr, error) {
	pipName := instance.ProviderID + "-pip"
	pip, err := a.pipClient.Get(ctx, a.resourceGroup, pipName, nil)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("failed to get public IP: %w", err)
	}
	if pip.Properties == nil || pip.Properties.IPAddress == nil {
		return netip.Addr{}, fmt.Errorf("public IP not yet assigned for %s", pipName)
	}
	return netip.ParseAddr(*pip.Properties.IPAddress)
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

func (a *azureProvider) DeleteInstance(ctx context.Context, instanceID providers.Instance) error {
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

func (a *azureProvider) ListInstances(ctx context.Context, region string) ([]providers.Instance, error) {
	vmName := fmt.Sprintf("tscloudvpn-%s", region)

	vm, err := a.computeClient.Get(ctx, a.resourceGroup, vmName, nil)
	if err != nil {
		var responseError *azcore.ResponseError
		if errors.As(err, &responseError) && responseError.StatusCode == 404 {
			return []providers.Instance{}, nil
		}
		return nil, err
	}

	// Verify this VM belongs to us by checking the owner tag
	if vm.Tags != nil {
		if owner, ok := vm.Tags[providers.OwnerTagKey]; ok && owner != nil && *owner != a.ownerID {
			// VM exists but belongs to different owner
			return []providers.Instance{}, nil
		}
	}

	var createdAt time.Time
	if vm.Properties != nil && vm.Properties.TimeCreated != nil {
		createdAt = *vm.Properties.TimeCreated
	}

	return []providers.Instance{
		{
			Hostname:     azureInstanceHostname(region),
			ProviderID:   vmName,
			ProviderName: providerName,
			CreatedAt:    createdAt,
			HourlyCost:   a.GetRegionHourlyEstimate(region),
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

func (a *azureProvider) GetRegionHourlyEstimate(region string) float64 {
	if err := a.ensureVMTypeCache(); err != nil {
		log.Printf("Failed to ensure VM type cache: %v", err)
		return 0
	}

	a.vmTypeCacheLock.RLock()
	defer a.vmTypeCacheLock.RUnlock()

	if types, ok := a.vmTypeCache[region]; ok && len(types) > 0 {
		return types[0].HourlyCost
	}
	return 0
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

	// Build region cache with real pricing from VM type cache
	regionCache := make(map[string]regionInfo)
	for _, location := range locations {
		regionCache[location.Code] = regionInfo{
			Code:        location.Code,
			LongName:    location.LongName,
			HourlyPrice: a.GetRegionHourlyEstimate(location.Code),
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

// fetchResourceSKUs fetches VM SKU metadata from the Azure Resource SKUs API.
// Returns a map of SKU name (e.g. "Standard_B1s") -> vmSKUInfo.
func (a *azureProvider) fetchResourceSKUs(ctx context.Context) (map[string]vmSKUInfo, error) {
	// Filter to a single location to avoid downloading 63K+ SKU entries globally.
	// SKU names and architectures are the same across regions.
	// Note: the Resource SKUs API only supports location filtering.
	filter := "location eq 'eastus'"
	pager := a.skuClient.NewListPager(&armcompute.ResourceSKUsClientListOptions{
		Filter: &filter,
	})

	skus := make(map[string]vmSKUInfo)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to get resource SKUs page: %w", err)
		}

		for _, sku := range page.Value {
			if sku.Name == nil {
				continue
			}
			name := *sku.Name
			if _, ok := skus[name]; ok {
				continue // Already seen this SKU
			}

			// Extract architecture from capabilities
			arm64 := false
			for _, cap := range sku.Capabilities {
				if cap.Name != nil && *cap.Name == "CpuArchitectureType" && cap.Value != nil && *cap.Value == "Arm64" {
					arm64 = true
					break
				}
			}

			skus[name] = vmSKUInfo{Arm64: arm64}
		}
	}

	log.Printf("Azure resource SKUs loaded: %d VM SKUs", len(skus))
	return skus, nil
}

// fetchRetailPrices fetches B-series Linux on-demand VM pricing from the Azure Retail Prices API.
// It cross-references with SKU info to determine architecture and filter to available SKUs.
// Returns a map of region -> []regionVMType sorted by price (cheapest first).
func (a *azureProvider) fetchRetailPrices(ctx context.Context, skuInfo map[string]vmSKUInfo) (map[string][]regionVMType, error) {
	// Server-side filter: B-series, Consumption, no Spot/Low Priority/Windows
	filter := "serviceName eq 'Virtual Machines'" +
		" and priceType eq 'Consumption'" +
		" and contains(meterName, 'Spot') eq false" +
		" and contains(meterName, 'Low Priority') eq false" +
		" and contains(productName, 'Windows') eq false" +
		" and contains(armSkuName, 'Standard_B')"

	u, err := url.Parse(retailPricesURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse retail prices URL: %w", err)
	}
	q := u.Query()
	q.Set("meterRegion", "'primary'")
	q.Set("$filter", filter)
	u.RawQuery = q.Encode()
	pageURL := u.String()

	// region -> armSkuName -> lowest price seen (for deduplication)
	type priceKey struct {
		region, sku string
	}
	type priceEntry struct {
		price float64
		arm64 bool
	}
	prices := make(map[priceKey]priceEntry)

	for pageURL != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch retail prices: %w", err)
		}

		var result retailPricesResponse
		err = json.NewDecoder(resp.Body).Decode(&result)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("failed to decode retail prices response: %w", err)
		}

		for _, item := range result.Items {
			if item.UnitPrice == 0 || item.ArmRegionName == "" || item.ArmSkuName == "" {
				continue
			}
			// Skip non-Linux OS-specific entries (RHEL, SUSE, etc.)
			if strings.Contains(item.ProductName, "RHEL") ||
				strings.Contains(item.ProductName, "SUSE") ||
				strings.Contains(item.ProductName, "Red Hat") {
				continue
			}

			// Look up architecture from Resource SKUs API
			info := skuInfo[item.ArmSkuName]

			key := priceKey{item.ArmRegionName, item.ArmSkuName}
			if existing, ok := prices[key]; !ok || item.UnitPrice < existing.price {
				prices[key] = priceEntry{price: item.UnitPrice, arm64: info.Arm64}
			}
		}

		pageURL = result.NextPageLink
	}

	// Group by region and sort by price
	regionMap := make(map[string][]regionVMType)
	for key, entry := range prices {
		regionMap[key.region] = append(regionMap[key.region], regionVMType{
			VMSize:     key.sku,
			HourlyCost: entry.price,
			Arm64:      entry.arm64,
		})
	}
	for region := range regionMap {
		sort.Slice(regionMap[region], func(i, j int) bool {
			return regionMap[region][i].HourlyCost < regionMap[region][j].HourlyCost
		})
	}

	log.Printf("Azure VM type cache populated: %d regions, %d total price entries", len(regionMap), len(prices))
	return regionMap, nil
}

func (a *azureProvider) ensureVMTypeCache() error {
	a.vmTypeCacheLock.Lock()
	defer a.vmTypeCacheLock.Unlock()

	if time.Since(a.vmTypeCacheTime) < cacheDuration && a.vmTypeCache != nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	skuInfo, err := a.fetchResourceSKUs(ctx)
	if err != nil {
		return fmt.Errorf("failed to fetch Azure resource SKUs: %w", err)
	}

	cache, err := a.fetchRetailPrices(ctx, skuInfo)
	if err != nil {
		return fmt.Errorf("failed to fetch Azure retail prices: %w", err)
	}

	a.vmTypeCache = cache
	a.vmTypeCacheTime = time.Now()
	return nil
}

func init() {
	providers.Register(providerName, "Azure", NewProvider)
}
