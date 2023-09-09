package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	html_template "html/template"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"text/template"
	"time"

	_ "embed"

	"github.com/anupcshan/tscloudvpn/cmd/tscloudvpn/assets"
	"github.com/anupcshan/tscloudvpn/internal/utils"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/bradenaw/juniper/container/tree"
	"github.com/bradenaw/juniper/xslices"
	"github.com/bradenaw/juniper/xsort"

	"github.com/tailscale/tailscale-client-go/tailscale"

	"tailscale.com/tsnet"
)

const (
	debianLatestImageSSMPath = "/aws/service/debian/release/12/latest/arm64"
)

var (
	//go:embed install.sh.tmpl
	initData string

	templates = html_template.Must(html_template.New("root").ParseFS(assets.Assets, "*.tmpl"))
)

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
	ssmClient := ssm.NewFromConfig(awsConfig)

	imageParam, err := ssmClient.GetParameter(ctx, &ssm.GetParameterInput{
		Name: aws.String(debianLatestImageSSMPath),
	})
	if err != nil {
		return err
	}

	logger.Printf("Found image id %s in region %s", aws.ToString(imageParam.Parameter.Value), awsConfig.Region)

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

func listRegions(ctx context.Context, awsConfig aws.Config) ([]string, error) {
	// Any region works. Pick something close to where this process is running to minimize latency.
	awsConfig.Region = "us-west-2"
	client := ec2.NewFromConfig(awsConfig)

	regionsResp, err := client.DescribeRegions(ctx, &ec2.DescribeRegionsInput{})
	if err != nil {
		return nil, err
	}

	var regions []string
	for _, region := range regionsResp.Regions {
		regions = append(regions, aws.ToString(region.RegionName))
	}

	sort.Strings(regions)

	return regions, nil
}

func Main() error {
	log.SetFlags(log.Lshortfile | log.Lmicroseconds)
	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()
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

	tsnetSrv := &tsnet.Server{
		Hostname:  "tscloudvpn",
		Ephemeral: true,
		AuthKey:   key.Key,
		Logf:      func(string, ...any) {}, // Silence logspam from tsnet
	}

	ln, err := tsnetSrv.Listen("tcp", ":80")
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
		tsnetSrv.Close()
	}()

	// go func() {
	// 	lc, _ := tsnetSrv.LocalClient()
	// 	for {
	// 		t := time.Now()
	// 		st, err := lc.Status(ctx)
	// 		if err != nil {
	// 			log.Println(err)
	// 			return
	// 		}

	// 		log.Printf("[%s] Status: %+v", time.Since(t), st)
	// 		for k, v := range st.Peer {
	// 			log.Printf("Peer %s -> %s %t", k, v.HostName, v.ExitNodeOption)
	// 		}
	// 		time.Sleep(time.Second)
	// 	}
	// }()

	lazyListRegions := utils.LazyWithErrors(
		func() ([]string, error) {
			return listRegions(ctx, awsConfig)
		},
	)

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
	mux.Handle("/", http.RedirectHandler("/regions", http.StatusTemporaryRedirect))
	mux.HandleFunc("/regions", func(w http.ResponseWriter, r *http.Request) {
		regions := lazyListRegions()

		devices, err := tsClient.Devices(ctx)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(err.Error()))
			return
		}

		deviceSet := tree.NewSet(xsort.OrderedLess[string])
		xslices.Map(devices, func(device tailscale.Device) string {
			deviceSet.Add(device.Hostname)
			return device.Hostname
		})

		type mappedRegion struct {
			Region  string
			HasNode bool
		}

		mappedRegions := xslices.Map(regions, func(region string) mappedRegion {
			return mappedRegion{
				Region:  region,
				HasNode: deviceSet.Contains(fmt.Sprintf("ec2-%s", region)),
			}
		})

		if err := templates.ExecuteTemplate(w, "list_regions.tmpl", mappedRegions); err != nil {
			w.Write([]byte(err.Error()))
		}
	})
	mux.Handle("/assets/", http.StripPrefix("/assets/", http.FileServer(http.FS(assets.Assets))))

	go func() {
		for _, region := range lazyListRegions() {
			region := region
			mux.HandleFunc(fmt.Sprintf("/regions/%s", region), func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					if r.PostFormValue("action") != "delete" {
						logger := log.New(io.MultiWriter(flushWriter{w}, os.Stderr), "", log.Lshortfile|log.Lmicroseconds)
						ctx, cancelFunc := context.WithTimeout(context.Background(), 5*time.Minute)
						defer cancelFunc()
						err := createInstance(ctx, logger, tsClient, awsConfig, region)
						if err != nil {
							w.Write([]byte(err.Error()))
						} else {
							w.Write([]byte("ok"))
						}
					} else {
						ctx := r.Context()
						devices, err := tsClient.Devices(ctx)
						if err != nil {
							w.WriteHeader(http.StatusInternalServerError)
							w.Write([]byte(err.Error()))
							return
						}

						filtered := xslices.Filter(devices, func(device tailscale.Device) bool {
							return device.Hostname == fmt.Sprintf("ec2-%s", region)
						})

						if len(filtered) > 0 {
							err := tsClient.DeleteDevice(ctx, filtered[0].ID)
							if err != nil {
								w.WriteHeader(http.StatusInternalServerError)
								w.Write([]byte(err.Error()))
								return
							} else {
								fmt.Fprint(w, "ok")
							}
						}
					}

					return
				}

				fmt.Fprintf(w, "Method %s not implemented", r.Method)
			})
		}
	}()

	return http.Serve(ln, mux)
}

func main() {
	if err := Main(); err != nil {
		log.Fatal(err)
	}
}
