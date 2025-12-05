package main

import (
	"context"
	"flag"
	"log"

	"github.com/anupcshan/tscloudvpn/internal/app"
	_ "github.com/anupcshan/tscloudvpn/internal/providers/azure"
	_ "github.com/anupcshan/tscloudvpn/internal/providers/digitalocean"
	_ "github.com/anupcshan/tscloudvpn/internal/providers/ec2"
	_ "github.com/anupcshan/tscloudvpn/internal/providers/gcp"
	_ "github.com/anupcshan/tscloudvpn/internal/providers/hetzner"
	_ "github.com/anupcshan/tscloudvpn/internal/providers/linode"
	_ "github.com/anupcshan/tscloudvpn/internal/providers/vultr"
)

var configFile string

func init() {
	flag.StringVar(&configFile, "config", "", "Path to config file (default: search in standard locations)")
}

func Main() error {
	ctx := context.Background()

	application, err := app.New(configFile)
	if err != nil {
		return err
	}

	if err := application.Initialize(ctx); err != nil {
		return err
	}

	return application.Run(ctx)
}

func main() {
	flag.Parse()

	if err := Main(); err != nil {
		log.Fatal(err)
	}
}
