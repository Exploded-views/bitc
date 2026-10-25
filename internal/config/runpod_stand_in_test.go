package config

// The real "runpod" type registers from internal/provider/runpod, which imports this package,
// so these in-package tests register a stand-in with the same endpoint rule.
func init() {
	RegisterProviderType("runpod", ProviderType{Endpoints: func(raw map[string]any) ([]string, error) {
		api, _ := raw["api_base"].(string)
		proxy, _ := raw["proxy_domain"].(string)
		return []string{firstNonEmpty(api, "https://api.runpod.io"), firstNonEmpty(proxy, "proxy.runpod.net")}, nil
	}})
}
