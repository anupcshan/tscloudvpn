package azure

import (
	"github.com/anupcshan/tscloudvpn/internal/providers"
)

func init() {
	// Ensure Azure provider is registered
	if _, exists := providers.ProviderFactoryRegistry["azure"]; !exists {
		panic("Azure provider not registered")
	}
}
