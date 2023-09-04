package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/template"
	"time"

	_ "embed"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/tailscale/tailscale-client-go/tailscale"

	"tailscale.com/tsnet"
)

const (
	debianOwnerId   = "136693071363"
	debianImageName = "debian-12-arm64-20230723-1450"
)

//go:embed install.sh.tmpl
var initData string

func createInstance(ctx context.Context, logger *log.Logger, tsClient *tailscale.Client, awsConfig aws.Config, region string) error {
	capabilities := tailscale.KeyCapabilities{}
	capabilities.Devices.Create.Tags = []string{"tag:untrusted"}
	capabilities.Devices.Create.Ephemeral = true
	capabilities.Devices.Create.Reusable = false
	capabilities.Devices.Create.Preauthorized = true
	key, err := tsClient.CreateKey(ctx, capabilities)
	if err != nil {
		return err
	}

	awsConfig.Region = region

	client := ec2.NewFromConfig(awsConfig)

	images, err := client.DescribeImages(ctx, &ec2.DescribeImagesInput{
		Filters: []types.Filter{
			{Name: aws.String("owner-id"), Values: []string{debianOwnerId}},
			{Name: aws.String("name"), Values: []string{debianImageName}},
		},
	})
	if err != nil {
		return err
	}

	if images == nil || len(images.Images) == 0 {
		return fmt.Errorf("No images found")
	}
	logger.Printf("Found image id %s in region %s", aws.ToString(images.Images[0].ImageId), awsConfig.Region)

	tmplOut := new(bytes.Buffer)
	hostname := fmt.Sprintf("ec2-%s", awsConfig.Region)
	if err := template.Must(template.New("tmpl").Parse(initData)).Execute(tmplOut, struct {
		Args string
	}{
		Args: fmt.Sprintf(
			`--advertise-tags="tag:untrusted" --authkey="%s" --hostname=%s`,
			key.Key,
			hostname,
		),
	}); err != nil {
		return err
	}

	launchTime := time.Now()

	input := &ec2.RunInstancesInput{
		ImageId:                           images.Images[0].ImageId,
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
		return err
	}

	logger.Printf("Launched instance %s", aws.ToString(output.Instances[0].InstanceId))
	logger.Printf("Waiting for instance to be allocated an IP address")

	for {
		status, err := client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
			InstanceIds: []string{aws.ToString(output.Instances[0].InstanceId)},
		})
		if err != nil {
			return err
		}

		ipAddr := aws.ToString(status.Reservations[0].Instances[0].PublicIpAddress)
		if ipAddr != "" {
			fmt.Println()
			logger.Printf("Instance IP: %s", ipAddr)
			break
		}

		time.Sleep(time.Second)
		fmt.Print(".")
	}

	logger.Printf("Waiting for instance to register on Tailscale")

	for {
		devices, err := tsClient.Devices(ctx)
		if err != nil {
			return err
		}

		var deviceId string
		var nodeName string
		for _, device := range devices {
			if device.Hostname == hostname && launchTime.Before(device.Created.Time) {
				deviceId = device.ID
				nodeName = strings.SplitN(device.Name, ".", 2)[0]
				fmt.Println()
				logger.Printf("Instance registered on Tailscale with ID %s, name %s", deviceId, nodeName)
				break
			}
		}

		if deviceId == "" {
			fmt.Print(".")
			time.Sleep(time.Second)
			continue
		}

		routes, err := tsClient.DeviceSubnetRoutes(ctx, deviceId)
		if err != nil {
			return err
		}

		logger.Printf("Approving exit node %s", nodeName)
		if err := tsClient.SetDeviceSubnetRoutes(ctx, deviceId, routes.Advertised); err != nil {
			return err
		}

		break
	}

	return nil
}

type flushWriter struct {
	w io.Writer
}

func (f flushWriter) Write(b []byte) (int, error) {
	n, err := f.w.Write(b)
	if flusher, ok := f.w.(http.Flusher); ok {
		flusher.Flush()
	}

	return n, err
}

func Main() error {
	log.SetFlags(log.Lshortfile | log.Lmicroseconds)
	ctx, cancelFunc := context.WithCancel(context.Background())
	awsConfig, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return err
	}

	oauthClientId := os.Getenv("TAILSCALE_CLIENT_ID")
	oauthSecret := os.Getenv("TAILSCALE_CLIENT_SECRET")
	tailnet := os.Getenv("TAILSCALE_TAILNET")

	tsClient, err := tailscale.NewClient(
		"",
		tailnet,
		tailscale.WithOAuthClientCredentials(
			oauthClientId,
			oauthSecret,
			[]string{"devices", "routes"},
		),
	)
	if err != nil {
		return err
	}

	capabilities := tailscale.KeyCapabilities{}
	capabilities.Devices.Create.Tags = []string{"tag:untrusted"}
	capabilities.Devices.Create.Ephemeral = true
	capabilities.Devices.Create.Reusable = true
	capabilities.Devices.Create.Preauthorized = true
	key, err := tsClient.CreateKey(ctx, capabilities)
	if err != nil {
		return err
	}

	s := &tsnet.Server{
		Hostname:  "tscloudvpn",
		Ephemeral: true,
		AuthKey:   key.Key,
		Logf:      func(string, ...any) {}, // Silence logspam from tsnet
	}

	ln, err := s.Listen("tcp", ":80")
	if err != nil {
		return err
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigs
		cancelFunc()
	}()

	defer cancelFunc()

	go func() {
		<-ctx.Done()
		ln.Close()
		s.Close()
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/create", func(w http.ResponseWriter, r *http.Request) {
		region := r.URL.Query().Get("region")
		logger := log.New(io.MultiWriter(flushWriter{w}, os.Stderr), "", log.Lshortfile|log.Lmicroseconds)
		err := createInstance(r.Context(), logger, tsClient, awsConfig, region)
		if err != nil {
			w.Write([]byte(err.Error()))
		} else {
			w.Write([]byte("ok"))
		}
	})

	return http.Serve(ln, mux)
}

func main() {
	if err := Main(); err != nil {
		log.Fatal(err)
	}
}
