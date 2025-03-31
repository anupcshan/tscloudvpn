package ec2

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"fmt"
	"log"
	"sort"
	"strings"
	"text/template"

	"github.com/anupcshan/tscloudvpn/internal/config"
	"github.com/anupcshan/tscloudvpn/internal/controlapi"
	"github.com/anupcshan/tscloudvpn/internal/providers"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
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
	cfg    aws.Config
	sshKey string
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

	return &ec2Provider{
		cfg:    awsCfg,
		sshKey: cfg.SSH.PublicKey,
	}, nil
}

func ec2InstanceHostname(region string) string {
	return fmt.Sprintf("ec2-%s", region)
}

func (e *ec2Provider) ListRegions(ctx context.Context) ([]providers.Region, error) {
	// Any region works. Pick something close to where this process is running to minimize latency.
	e.cfg.Region = "us-west-2"
	client := ec2.NewFromConfig(e.cfg)
	ssmClient := ssm.NewFromConfig(e.cfg)

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

func (e *ec2Provider) CreateInstance(ctx context.Context, region string, key *controlapi.PreauthKey) (string, error) {
	e.cfg.Region = region

	client := ec2.NewFromConfig(e.cfg)
	ssmClient := ssm.NewFromConfig(e.cfg)

	imageParam, err := ssmClient.GetParameter(ctx, &ssm.GetParameterInput{
		Name: aws.String(debianLatestImageSSMPath),
	})
	if err != nil {
		return "", err
	}

	log.Printf("Found image id %s in region %s", aws.ToString(imageParam.Parameter.Value), e.cfg.Region)

	tmplOut := new(bytes.Buffer)
	hostname := ec2InstanceHostname(e.cfg.Region)
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
		return "", err
	}

	// Get or create security group
	sgID, err := e.getOrCreateSecurityGroup(ctx, client)
	if err != nil {
		return "", fmt.Errorf("failed to setup security group: %v", err)
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
				},
			},
		},
		UserData: aws.String(base64.StdEncoding.EncodeToString(tmplOut.Bytes())),
	}

	output, err := client.RunInstances(ctx, input)
	if err != nil {
		return "", err
	}
	log.Printf("Launched instance %s", aws.ToString(output.Instances[0].InstanceId))

	return hostname, nil
}

func (e *ec2Provider) GetInstanceStatus(ctx context.Context, region string) (providers.InstanceStatus, error) {
	e.cfg.Region = region

	client := ec2.NewFromConfig(e.cfg)
	listInstances, err := client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []types.Filter{
			{
				Name:   aws.String("tag-key"),
				Values: []string{"tscloudvpn"},
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

func (e *ec2Provider) Hostname(region string) providers.HostName {
	return providers.HostName(ec2InstanceHostname(region))
}

// GetRegionPrice returns the hourly price for the t4g.nano instance in the specified region
func (e *ec2Provider) GetRegionPrice(region string) float64 {
	// EC2 t4g.nano instance prices vary by region
	// These prices are subject to change; in a production environment,
	// consider using the AWS Price List API to get up-to-date pricing
	prices := map[string]float64{
		"us-east-1":      0.0042, // N. Virginia
		"us-east-2":      0.0042, // Ohio
		"us-west-1":      0.0051, // N. California
		"us-west-2":      0.0042, // Oregon
		"ap-south-1":     0.0046, // Mumbai
		"ap-northeast-1": 0.0055, // Tokyo
		"ap-northeast-2": 0.0048, // Seoul
		"ap-northeast-3": 0.0055, // Osaka
		"ap-southeast-1": 0.0048, // Singapore
		"ap-southeast-2": 0.0048, // Sydney
		"ca-central-1":   0.0047, // Canada
		"eu-central-1":   0.0048, // Frankfurt
		"eu-west-1":      0.0042, // Ireland
		"eu-west-2":      0.0045, // London
		"eu-west-3":      0.0045, // Paris
		"eu-north-1":     0.0039, // Stockholm
		"sa-east-1":      0.0064, // São Paulo
	}

	if price, ok := prices[region]; ok {
		return price
	}
	return 0.0042 // Default price if region not found
}

func init() {
	providers.Register(providerName, NewProvider)
}
