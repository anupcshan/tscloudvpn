package config

import (
	"os"
	"path/filepath"

	"github.com/anupcshan/tscloudvpn/internal/controlapi"
	"gopkg.in/yaml.v3"
)

type Config struct {
	SSH struct {
		PublicKey string `yaml:"public_key"`
	} `yaml:"ssh"`

	Control struct {
		Type      string `yaml:"type"` // "tailscale" or "headscale"
		Tailscale struct {
			ClientID     string `yaml:"client_id"`
			ClientSecret string `yaml:"client_secret"`
			Tailnet      string `yaml:"tailnet"`
		} `yaml:"tailscale"`
		Headscale struct {
			API    string `yaml:"api"`
			URL    string `yaml:"url"`
			APIKey string `yaml:"api_key"`
			User   string `yaml:"user"`
		} `yaml:"headscale"`
	} `yaml:"control"`

	Providers struct {
		DigitalOcean struct {
			Token string `yaml:"token"`
		} `yaml:"digitalocean"`
		GCP struct {
			CredentialsJSON string `yaml:"credentials_json"`
			ProjectID       string `yaml:"project_id"`
			ServiceAccount  string `yaml:"service_account"`
		} `yaml:"gcp"`
		Vultr struct {
			APIKey string `yaml:"api_key"`
		} `yaml:"vultr"`
		Linode struct {
			Token string `yaml:"token"`
		} `yaml:"linode"`
		AWS struct {
			AccessKey       string `yaml:"access_key"`        // Optional, can use environment or ~/.aws/credentials
			SecretKey       string `yaml:"secret_key"`        // Optional, can use environment or ~/.aws/credentials
			SessionToken    string `yaml:"session_token"`     // Optional, can use environment or ~/.aws/credentials
			SharedConfigDir string `yaml:"shared_config_dir"` // Optional, defaults to ~/.aws
		} `yaml:"aws"`
		// Add other providers as needed
	} `yaml:"providers"`
}

func (cfg *Config) GetController() (controlapi.ControlApi, error) {
	if cfg.Control.Type == "headscale" {
		return controlapi.NewHeadscaleClient(
			cfg.Control.Headscale.API,
			cfg.Control.Headscale.URL,
			cfg.Control.Headscale.APIKey,
			cfg.Control.Headscale.User,
		)
	}

	return controlapi.NewTailscaleClient(
		cfg.Control.Tailscale.Tailnet,
		cfg.Control.Tailscale.ClientID,
		cfg.Control.Tailscale.ClientSecret,
	)
}

// LoadConfig loads configuration from the specified file path
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, err
	}

	return &config, nil
}

// LoadDefaultConfig attempts to load configuration from default locations
func LoadDefaultConfig() (*Config, error) {
	// Try XDG config directory first
	if xdgConfig := os.Getenv("XDG_CONFIG_HOME"); xdgConfig != "" {
		path := filepath.Join(xdgConfig, "tscloudvpn", "config.yaml")
		if _, err := os.Stat(path); err == nil {
			return LoadConfig(path)
		}
	}

	// Fallback to home directory
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	path := filepath.Join(home, ".config", "tscloudvpn", "config.yaml")
	if _, err := os.Stat(path); err == nil {
		return LoadConfig(path)
	}

	// Legacy location in home directory
	path = filepath.Join(home, ".tscloudvpn.yaml")
	if _, err := os.Stat(path); err == nil {
		return LoadConfig(path)
	}

	return nil, os.ErrNotExist
}

// LoadFromEnv creates a Config from environment variables (for backward compatibility)
func LoadFromEnv() *Config {
	var cfg Config

	cfg.SSH.PublicKey = os.Getenv("SSH_PUBKEY")

	cfg.Control.Type = "tailscale"
	if os.Getenv("HEADSCALE_API") != "" {
		cfg.Control.Type = "headscale"
	}

	cfg.Control.Tailscale.ClientID = os.Getenv("TAILSCALE_CLIENT_ID")
	cfg.Control.Tailscale.ClientSecret = os.Getenv("TAILSCALE_CLIENT_SECRET")
	cfg.Control.Tailscale.Tailnet = os.Getenv("TAILSCALE_TAILNET")

	cfg.Control.Headscale.API = os.Getenv("HEADSCALE_API")
	cfg.Control.Headscale.URL = os.Getenv("HEADSCALE_URL")
	cfg.Control.Headscale.APIKey = os.Getenv("HEADSCALE_APIKEY")
	cfg.Control.Headscale.User = os.Getenv("HEADSCALE_USER")

	cfg.Providers.DigitalOcean.Token = os.Getenv("DIGITALOCEAN_TOKEN")

	// GCP configuration
	if gcpCredentialsFile := os.Getenv("GCP_CREDENTIALS_JSON_FILE"); gcpCredentialsFile != "" {
		if credJSON, err := os.ReadFile(gcpCredentialsFile); err == nil {
			cfg.Providers.GCP.CredentialsJSON = string(credJSON)
			cfg.Providers.GCP.ProjectID = os.Getenv("GCP_PROJECT_ID")
			cfg.Providers.GCP.ServiceAccount = os.Getenv("GCP_SERVICE_ACCOUNT")
		}
	}

	// Vultr configuration
	cfg.Providers.Vultr.APIKey = os.Getenv("VULTR_API_KEY")

	// Linode configuration
	cfg.Providers.Linode.Token = os.Getenv("LINODE_TOKEN")

	// AWS configuration will be handled by the AWS SDK directly from environment variables and ~/.aws/credentials

	return &cfg
}
