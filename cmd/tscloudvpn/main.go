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

var (
	configFile         string
	enableGCDeletion   bool
)

func init() {
	flag.StringVar(&configFile, "config", "", "Path to config file (default: search in standard locations)")
	flag.BoolVar(&enableGCDeletion, "enable-gc-deletion", false, "Enable garbage collector to delete orphaned cloud instances (default: dry-run mode)")
}

func Main() error {
	ctx := context.Background()

	application, err := app.New(configFile, app.Options{
		EnableGCDeletion: enableGCDeletion,
	})
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
