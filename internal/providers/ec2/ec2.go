package ec2

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"text/template"

	"github.com/anupcshan/tscloudvpn/internal/config"
	"github.com/anupcshan/tscloudvpn/internal/controlapi"
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
	debianLatestImageSSMPath = "/aws/service/debian/release/12/latest/arm64"
	providerName             = "ec2"
	securityGroupName        = "tscloudvpn-sg"
	// Tailscale uses this port for inbound connections to the Tailscaled - see https://tailscale.com/kb/1257/connection-types#hard-nat
	tailscaledInboundPort = 41641
)

type ec2Provider struct {
	mu             sync.Mutex
	listPricesOnce sync.Once
	regionPriceMap map[string]float64
	cfg            aws.Config
	sshKey         string
	ownerID        string // Unique identifier for this tscloudvpn instance
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
		sshKey:  cfg.SSH.PublicKey,
		ownerID: providers.GetOwnerID(cfg),
	}

	go e.populatePriceCache()
	return e, nil
}

func ec2InstanceHostname(region string) string {
	return fmt.Sprintf("ec2-%s", region)
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

func (e *ec2Provider) DeleteInstance(ctx context.Context, instanceID providers.InstanceID) error {
	e.mu.Lock()
	// Extract region from hostname (e.g., "ec2-us-west-2" -> "us-west-2")
	region := strings.TrimPrefix(instanceID.Hostname, "ec2-")
	e.cfg.Region = region
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

func (e *ec2Provider) CreateInstance(ctx context.Context, region string, key *controlapi.PreauthKey) (providers.InstanceID, error) {
	e.mu.Lock()
	e.cfg.Region = region
	client := ec2.NewFromConfig(e.cfg)
	ssmClient := ssm.NewFromConfig(e.cfg)
	e.mu.Unlock()

	imageParam, err := ssmClient.GetParameter(ctx, &ssm.GetParameterInput{
		Name: aws.String(debianLatestImageSSMPath),
	})
	if err != nil {
		return providers.InstanceID{}, err
	}

	log.Printf("Found image id %s in region %s", aws.ToString(imageParam.Parameter.Value), region)

	tmplOut := new(bytes.Buffer)
	hostname := ec2InstanceHostname(region)
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
		OnExit: "sudo /sbin/poweroff",
		SSHKey: e.sshKey,
	}); err != nil {
		return providers.InstanceID{}, err
	}

	// Get or create security group
	sgID, err := e.getOrCreateSecurityGroup(ctx, client)
	if err != nil {
		return providers.InstanceID{}, fmt.Errorf("failed to setup security group: %v", err)
	}

	input := &ec2.RunInstancesInput{
		ImageId:                           imageParam.Parameter.Value,
		InstanceType:                      types.InstanceTypeT4gNano,
		MinCount:                          aws.Int32(1),
		MaxCount:                          aws.Int32(1),
		InstanceInitiatedShutdownBehavior: types.ShutdownBehaviorTerminate,
		SecurityGroupIds:                  []string{sgID},
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: "instance",
				Tags: []types.Tag{
					{Key: aws.String("tscloudvpn"), Value: aws.String("true")},
					{Key: aws.String(providers.OwnerTagKey), Value: aws.String(e.ownerID)},
				},
			},
		},
		UserData: aws.String(base64.StdEncoding.EncodeToString(tmplOut.Bytes())),
	}

	output, err := client.RunInstances(ctx, input)
	if err != nil {
		return providers.InstanceID{}, err
	}
	log.Printf("Launched instance %s", aws.ToString(output.Instances[0].InstanceId))

	return providers.InstanceID{
		Hostname:     hostname,
		ProviderID:   aws.ToString(output.Instances[0].InstanceId),
		ProviderName: "ec2",
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

func (e *ec2Provider) ListInstances(ctx context.Context, region string) ([]providers.InstanceID, error) {
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

	var instanceIDs []providers.InstanceID
	for _, reservation := range listInstances.Reservations {
		for _, instance := range reservation.Instances {
			if instance.State.Name != types.InstanceStateNameTerminated {
				instanceIDs = append(instanceIDs, providers.InstanceID{
					Hostname:     ec2InstanceHostname(region),
					ProviderID:   aws.ToString(instance.InstanceId),
					ProviderName: providerName,
					CreatedAt:    aws.ToTime(instance.LaunchTime),
				})
			}
		}
	}

	return instanceIDs, nil
}

func (e *ec2Provider) Hostname(region string) providers.HostName {
	return providers.HostName(ec2InstanceHostname(region))
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

func (e *ec2Provider) populatePriceCache() {
	e.listPricesOnce.Do(func() {
		ctx := context.Background()
		e.mu.Lock()
		client := pricing.NewFromConfig(e.cfg, withRegion("us-east-1"))
		e.mu.Unlock()

		e.regionPriceMap = make(map[string]float64)
		paginator := pricing.NewGetProductsPaginator(client, &pricing.GetProductsInput{
			ServiceCode: aws.String("AmazonEC2"),
			Filters: []pricingtypes.Filter{
				{
					Type:  pricingtypes.FilterTypeTermMatch,
					Field: aws.String("instanceType"),
					Value: aws.String("t4g.nano"),
				},
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
			},
		})

		for paginator.HasMorePages() {
			page, err := paginator.NextPage(ctx)
			if err != nil {
				log.Printf("Error getting product list: %v", err)
				return
			}

			for _, product := range page.PriceList {
				var p productType
				if err := json.Unmarshal([]byte(product), &p); err != nil {
					log.Printf("Error unmarshalling product: %v", err)
					continue
				}
				for _, term := range p.Terms.OnDemand {
					for _, price := range term.PriceDimensions {
						e.regionPriceMap[p.Product.Attributes["regionCode"]] = price.PricePerUnit.USD
					}
				}

			}
		}

		log.Printf("EC2 region price cache populated with %d regions", len(e.regionPriceMap))
	})
}

// GetRegionPrice returns the hourly price for the t4g.nano instance in the specified region
func (e *ec2Provider) GetRegionPrice(region string) float64 {
	e.populatePriceCache()

	return e.regionPriceMap[region]
}

func init() {
	providers.Register(providerName, NewProvider)
}
