package ec2

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/anupcshan/tscloudvpn/internal/config"
	"github.com/anupcshan/tscloudvpn/internal/providers"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
	pricingtypes "github.com/aws/aws-sdk-go-v2/service/pricing/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"golang.org/x/exp/slices"
)

const (
	ubuntuLatestImageSSMPath = "/aws/service/canonical/ubuntu/server/24.04/stable/current/arm64/hvm/ebs-gp3/ami-id"
	providerName             = "ec2"
	securityGroupName        = "tscloudvpn-sg"
	// Tailscale uses this port for inbound connections to the Tailscaled - see https://tailscale.com/kb/1257/connection-types#hard-nat
	tailscaledInboundPort = 41641
)

// instanceTypeSpec holds the specs and pricing for one instance type in one region.
type instanceTypeSpec struct {
	Name       string
	VCPUs      int
	RamMB      int
	HourlyCost float64
}

type ec2Provider struct {
	mu                  sync.Mutex
	listPricesOnce      sync.Once
	regionInstanceTypes map[string][]instanceTypeSpec // region -> sorted by cost (cheapest first)
	cfg                 aws.Config
	ownerID             string // Unique identifier for this tscloudvpn instance
}

func NewProvider(ctx context.Context, cfg *config.Config) (providers.Provider, error) {
	// Build AWS config options from our config file settings
	var opts []func(*awsconfig.LoadOptions) error

	// Pick any region - we set the region when we need to use it
	opts = append(opts, awsconfig.WithDefaultRegion("us-west-2"))

	if cfg.Providers.AWS.SharedConfigDir != "" {
		opts = append(opts, awsconfig.WithSharedConfigFiles([]string{
			fmt.Sprintf("%s/credentials", cfg.Providers.AWS.SharedConfigDir),
			fmt.Sprintf("%s/config", cfg.Providers.AWS.SharedConfigDir),
		}))
	}

	if cfg.Providers.AWS.AccessKey != "" && cfg.Providers.AWS.SecretKey != "" {
		// Use static credentials from config
		opts = append(opts, awsconfig.WithCredentialsProvider(aws.CredentialsProviderFunc(
			func(ctx context.Context) (aws.Credentials, error) {
				return aws.Credentials{
					AccessKeyID:     cfg.Providers.AWS.AccessKey,
					SecretAccessKey: cfg.Providers.AWS.SecretKey,
					SessionToken:    cfg.Providers.AWS.SessionToken,
				}, nil
			},
		)))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, err
	}

	if _, err := awsCfg.Credentials.Retrieve(ctx); err != nil {
		// No credentials set. Nothing to do
		return nil, nil
	}

	e := &ec2Provider{
		cfg:     awsCfg,
		ownerID: providers.GetOwnerID(cfg),
	}

	go e.populatePriceCache()
	return e, nil
}

func ec2InstanceHostname(region string) string {
	return fmt.Sprintf("ec2-%s", region)
}

func (e *ec2Provider) buildTags(extra map[string]string) []types.Tag {
	tags := []types.Tag{
		{Key: aws.String("tscloudvpn"), Value: aws.String("true")},
		{Key: aws.String(providers.OwnerTagKey), Value: aws.String(e.ownerID)},
	}
	for k, v := range extra {
		tags = append(tags, types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return tags
}

func (e *ec2Provider) ListRegions(ctx context.Context) ([]providers.Region, error) {
	// Any region works. Pick something close to where this process is running to minimize latency.
	e.mu.Lock()
	e.cfg.Region = "us-west-2"
	client := ec2.NewFromConfig(e.cfg)
	ssmClient := ssm.NewFromConfig(e.cfg)
	e.mu.Unlock()

	regionsResp, err := client.DescribeRegions(ctx, &ec2.DescribeRegionsInput{})
	if err != nil {
		return nil, err
	}

	var regions []providers.Region
	for _, region := range regionsResp.Regions {
		location, err := ssmClient.GetParameter(ctx, &ssm.GetParameterInput{
			Name: aws.String(fmt.Sprintf("/aws/service/global-infrastructure/regions/%s/longName", *region.RegionName)),
		})
		if err != nil {
			return nil, err
		}
		regions = append(regions, providers.Region{
			Code:     aws.ToString(region.RegionName),
			LongName: aws.ToString(location.Parameter.Value),
		})
	}

	sort.Slice(regions, func(i, j int) bool {
		return regions[i].Code < regions[j].Code
	})

	return regions, nil
}

func hasVPNPortRule(group types.SecurityGroup) bool {
	for _, permission := range group.IpPermissions {
		if aws.ToString(permission.IpProtocol) == "udp" &&
			aws.ToInt32(permission.FromPort) == tailscaledInboundPort &&
			aws.ToInt32(permission.ToPort) == tailscaledInboundPort {
			for _, ipRange := range permission.IpRanges {
				if aws.ToString(ipRange.CidrIp) == "0.0.0.0/0" {
					return true
				}
			}
		}
	}
	return false
}

func (e *ec2Provider) getOrCreateSecurityGroup(ctx context.Context, client *ec2.Client) (string, error) {
	// First try to find existing security group
	if describeResult, err := client.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		Filters: []types.Filter{
			{
				Name:   aws.String("group-name"),
				Values: []string{securityGroupName},
			},
		},
	}); err != nil {
		return "", fmt.Errorf("failed to describe security groups: %v", err)
	} else if len(describeResult.SecurityGroups) > 0 {
		sg := describeResult.SecurityGroups[0]
		if !hasVPNPortRule(sg) {
			if err := e.addVPNPortRule(ctx, client, sg.GroupId); err != nil {
				return "", fmt.Errorf("failed to authorize security group ingress for existing group: %v", err)
			}
		}
		return *sg.GroupId, nil
	}

	// Create new security group
	if createResult, err := client.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName:   aws.String(securityGroupName),
		Description: aws.String("Security group for tscloudvpn instances"),
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeSecurityGroup,
				Tags: []types.Tag{
					{Key: aws.String("tscloudvpn"), Value: aws.String("true")},
				},
			},
		},
	}); err != nil {
		return "", fmt.Errorf("failed to create security group: %v", err)
	} else {
		if err := e.addVPNPortRule(ctx, client, createResult.GroupId); err != nil {
			return "", fmt.Errorf("failed to authorize security group ingress: %v", err)
		}

		return *createResult.GroupId, nil
	}
}

func (e *ec2Provider) addVPNPortRule(ctx context.Context, client *ec2.Client, groupId *string) error {
	_, err := client.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: groupId,
		IpPermissions: []types.IpPermission{
			{
				IpProtocol: aws.String("udp"),
				FromPort:   aws.Int32(tailscaledInboundPort),
				ToPort:     aws.Int32(tailscaledInboundPort),
				IpRanges: []types.IpRange{
					{
						CidrIp:      aws.String("0.0.0.0/0"),
						Description: aws.String("VPN port access"),
					},
				},
			},
		},
	})
	return err
}

func (e *ec2Provider) addSSHPortRule(ctx context.Context, client *ec2.Client, groupId *string) error {
	_, err := client.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: groupId,
		IpPermissions: []types.IpPermission{
			{
				IpProtocol: aws.String("tcp"),
				FromPort:   aws.Int32(22),
				ToPort:     aws.Int32(22),
				IpRanges: []types.IpRange{
					{
						CidrIp:      aws.String("0.0.0.0/0"),
						Description: aws.String("SSH access for debug diagnostics"),
					},
				},
			},
		},
	})
	return err
}

func (e *ec2Provider) DebugSSHUser() string { return "root" }

func (e *ec2Provider) GetPublicIP(ctx context.Context, instance providers.Instance) (netip.Addr, error) {
	e.mu.Lock()
	e.cfg.Region = instance.Region
	client := ec2.NewFromConfig(e.cfg)
	e.mu.Unlock()

	result, err := client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instance.ProviderID},
	})
	if err != nil {
		return netip.Addr{}, fmt.Errorf("failed to describe instance: %w", err)
	}

	for _, reservation := range result.Reservations {
		for _, inst := range reservation.Instances {
			if inst.PublicIpAddress != nil {
				addr, err := netip.ParseAddr(*inst.PublicIpAddress)
				if err != nil {
					return netip.Addr{}, fmt.Errorf("failed to parse IP %q: %w", *inst.PublicIpAddress, err)
				}
				return addr, nil
			}
		}
	}
	return netip.Addr{}, fmt.Errorf("no public IP found for instance %s", instance.ProviderID)
}

func (e *ec2Provider) DeleteInstance(ctx context.Context, instanceID providers.Instance) error {
	e.mu.Lock()
	e.cfg.Region = instanceID.Region
	client := ec2.NewFromConfig(e.cfg)
	e.mu.Unlock()

	_, err := client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: []string{instanceID.ProviderID},
	})
	if err != nil {
		return fmt.Errorf("failed to terminate instance: %w", err)
	}

	log.Printf("Terminated instance %s", instanceID.ProviderID)
	return nil
}

func (e *ec2Provider) CreateInstance(ctx context.Context, req providers.CreateRequest) (providers.Instance, error) {
	e.mu.Lock()
	e.cfg.Region = req.Region
	client := ec2.NewFromConfig(e.cfg)
	ssmClient := ssm.NewFromConfig(e.cfg)
	e.mu.Unlock()

	imageParam, err := ssmClient.GetParameter(ctx, &ssm.GetParameterInput{
		Name: aws.String(ubuntuLatestImageSSMPath),
	})
	if err != nil {
		return providers.Instance{}, err
	}

	log.Printf("Found image id %s in region %s", aws.ToString(imageParam.Parameter.Value), req.Region)

	// Get or create security group
	sgID, err := e.getOrCreateSecurityGroup(ctx, client)
	if err != nil {
		return providers.Instance{}, fmt.Errorf("failed to setup security group: %v", err)
	}

	if req.Debug {
		if err := e.addSSHPortRule(ctx, client, &sgID); err != nil {
			log.Printf("Failed to add SSH port rule (may already exist): %v", err)
		}
	}

	input := &ec2.RunInstancesInput{
		ImageId:                           imageParam.Parameter.Value,
		InstanceType:                      e.cheapestInstanceType(req.Region),
		MinCount:                          aws.Int32(1),
		MaxCount:                          aws.Int32(1),
		InstanceInitiatedShutdownBehavior: types.ShutdownBehaviorTerminate,
		SecurityGroupIds:                  []string{sgID},
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: "instance",
				Tags:         e.buildTags(req.Tags),
			},
		},
		UserData: aws.String(base64.StdEncoding.EncodeToString([]byte(req.UserData))),
	}

	output, err := client.RunInstances(ctx, input)
	if err != nil {
		return providers.Instance{}, err
	}
	log.Printf("Launched instance %s", aws.ToString(output.Instances[0].InstanceId))

	return providers.Instance{
		Hostname:     req.Hostname,
		ProviderID:   aws.ToString(output.Instances[0].InstanceId),
		ProviderName: "ec2",
		Region:       req.Region,
		HourlyCost:   e.GetRegionHourlyEstimate(req.Region),
	}, nil
}

func (e *ec2Provider) GetInstanceStatus(ctx context.Context, region string) (providers.InstanceStatus, error) {
	e.mu.Lock()
	e.cfg.Region = region
	client := ec2.NewFromConfig(e.cfg)
	e.mu.Unlock()

	listInstances, err := client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []types.Filter{
			{
				Name:   aws.String("tag-key"),
				Values: []string{"tscloudvpn"},
			},
			{
				Name:   aws.String("tag:" + providers.OwnerTagKey),
				Values: []string{e.ownerID},
			},
		},
	})

	if err != nil {
		return providers.InstanceStatusMissing, err
	}

	var instances []types.Instance
	for _, reservation := range listInstances.Reservations {
		instances = append(instances, reservation.Instances...)
	}

	slices.SortFunc(instances, func(i, j types.Instance) int {
		return -aws.ToTime(i.LaunchTime).Compare(aws.ToTime(j.LaunchTime))
	})

	for _, instance := range instances {
		if instance.State.Name != types.InstanceStateNameTerminated {
			return providers.InstanceStatusRunning, nil
		}
	}

	return providers.InstanceStatusMissing, err
}

func (e *ec2Provider) ListInstances(ctx context.Context, region string) ([]providers.Instance, error) {
	e.mu.Lock()
	e.cfg.Region = region
	client := ec2.NewFromConfig(e.cfg)
	e.mu.Unlock()

	listInstances, err := client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []types.Filter{
			{
				Name:   aws.String("tag-key"),
				Values: []string{"tscloudvpn"},
			},
			{
				Name:   aws.String("tag:" + providers.OwnerTagKey),
				Values: []string{e.ownerID},
			},
		},
	})

	if err != nil {
		return nil, err
	}

	var instanceIDs []providers.Instance
	for _, reservation := range listInstances.Reservations {
		for _, instance := range reservation.Instances {
			if instance.State.Name != types.InstanceStateNameTerminated {
				hostname := ec2InstanceHostname(region)
				for _, tag := range instance.Tags {
					if aws.ToString(tag.Key) == providers.NameTagKey {
						hostname = aws.ToString(tag.Value)
						break
					}
				}
				instanceIDs = append(instanceIDs, providers.Instance{
					Hostname:     hostname,
					ProviderID:   aws.ToString(instance.InstanceId),
					ProviderName: providerName,
					Region:       region,
					CreatedAt:    aws.ToTime(instance.LaunchTime),
					HourlyCost:   e.GetRegionHourlyEstimate(region),
				})
			}
		}
	}

	return instanceIDs, nil
}

func withRegion(region string) func(options *pricing.Options) {
	return func(options *pricing.Options) {
		options.Region = region
	}
}

type productType struct {
	Product struct {
		Attributes map[string]string `json:"attributes"`
	} `json:"product"`
	Terms struct {
		OnDemand map[string]struct {
			PriceDimensions map[string]struct {
				PricePerUnit struct {
					USD float64 `json:"USD,string"`
				} `json:"pricePerUnit"`
			} `json:"priceDimensions"`
		} `json:"OnDemand"`
	} `json:"terms"`
}

// parseMemoryMB parses an AWS memory string like "0.5 GiB" into megabytes.
func parseMemoryMB(s string) int {
	s = strings.TrimSuffix(s, " GiB")
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return int(f * 1024)
}

func (e *ec2Provider) populatePriceCache() {
	e.listPricesOnce.Do(func() {
		ctx := context.Background()
		e.mu.Lock()
		client := pricing.NewFromConfig(e.cfg, withRegion("us-east-1"))
		e.mu.Unlock()

		regionTypes := make(map[string][]instanceTypeSpec)

		paginator := pricing.NewGetProductsPaginator(client, &pricing.GetProductsInput{
			ServiceCode: aws.String("AmazonEC2"),
			MaxResults:  aws.Int32(100),
			Filters: []pricingtypes.Filter{
				{
					Type:  pricingtypes.FilterTypeTermMatch,
					Field: aws.String("operatingSystem"),
					Value: aws.String("Linux"),
				},
				{
					Type:  pricingtypes.FilterTypeTermMatch,
					Field: aws.String("capacitystatus"),
					Value: aws.String("UnusedCapacityReservation"),
				},
				{
					Type:  pricingtypes.FilterTypeTermMatch,
					Field: aws.String("tenancy"),
					Value: aws.String("Shared"),
				},
				{
					Type:  pricingtypes.FilterTypeTermMatch,
					Field: aws.String("preInstalledSw"),
					Value: aws.String("NA"),
				},
				{
					Type:  pricingtypes.FilterTypeTermMatch,
					Field: aws.String("locationType"),
					Value: aws.String("AWS Region"),
				},
				{
					Type:  pricingtypes.FilterTypeTermMatch,
					Field: aws.String("currentGeneration"),
					Value: aws.String("Yes"),
				},
			},
		})

		pages := 0
		for paginator.HasMorePages() {
			page, err := paginator.NextPage(ctx)
			if err != nil {
				log.Printf("Error getting EC2 pricing (page %d): %v", pages, err)
				break
			}
			pages++

			for _, product := range page.PriceList {
				var p productType
				if err := json.Unmarshal([]byte(product), &p); err != nil {
					log.Printf("Error unmarshalling product: %v", err)
					continue
				}
				region := p.Product.Attributes["regionCode"]
				instanceType := p.Product.Attributes["instanceType"]
				vcpus, _ := strconv.Atoi(p.Product.Attributes["vcpu"])
				ramMB := parseMemoryMB(p.Product.Attributes["memory"])

				for _, term := range p.Terms.OnDemand {
					for _, price := range term.PriceDimensions {
						if price.PricePerUnit.USD > 0 {
							regionTypes[region] = append(regionTypes[region], instanceTypeSpec{
								Name:       instanceType,
								VCPUs:      vcpus,
								RamMB:      ramMB,
								HourlyCost: price.PricePerUnit.USD,
							})
						}
					}
				}
			}
		}

		// Sort each region's types by cost (cheapest first)
		for region := range regionTypes {
			sort.Slice(regionTypes[region], func(i, j int) bool {
				return regionTypes[region][i].HourlyCost < regionTypes[region][j].HourlyCost
			})
		}

		e.regionInstanceTypes = regionTypes
		entries := 0
		for _, v := range regionTypes {
			entries += len(v)
		}
		log.Printf("EC2 price cache populated: %d pages, %d regions, %d entries", pages, len(regionTypes), entries)
	})
}

// GetRegionHourlyEstimate returns the hourly price for the cheapest instance type in the specified region.
func (e *ec2Provider) GetRegionHourlyEstimate(region string) float64 {
	e.populatePriceCache()

	specs := e.regionInstanceTypes[region]
	if len(specs) == 0 {
		return 0
	}
	return specs[0].HourlyCost
}

// cheapestInstanceType returns the cheapest instance type name for a region,
// falling back to t4g.nano if the price cache isn't populated.
func (e *ec2Provider) cheapestInstanceType(region string) types.InstanceType {
	e.populatePriceCache()

	specs := e.regionInstanceTypes[region]
	if len(specs) == 0 {
		return types.InstanceTypeT4gNano
	}
	return types.InstanceType(specs[0].Name)
}

func init() {
	providers.Register(providerName, "AWS EC2", NewProvider)
}
