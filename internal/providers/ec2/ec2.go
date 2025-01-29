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

	input := &ec2.RunInstancesInput{
		ImageId:                           imageParam.Parameter.Value,
		InstanceType:                      types.InstanceTypeT4gNano,
		MinCount:                          aws.Int32(1),
		MaxCount:                          aws.Int32(1),
		InstanceInitiatedShutdownBehavior: types.ShutdownBehaviorTerminate,
		// To debug:
		// KeyName:                           aws.String("ssh-key-name"),
		// SecurityGroupIds:                  []string{"sg-..."},
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

func init() {
	providers.Register(providerName, NewProvider)
}
