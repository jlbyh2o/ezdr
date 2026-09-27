package portal

import (
	"net/netip"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadConfigDefaults(t *testing.T) {
	c, err := LoadConfig(env(map[string]string{"EZDR_PUBLIC_URL": "https://portal.example.com/"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicURL.String() != "https://portal.example.com" {
		t.Errorf("PublicURL = %s", c.PublicURL)
	}
	if c.WireGuardEndpoint != "portal.example.com:51820" {
		t.Errorf("WireGuardEndpoint = %s", c.WireGuardEndpoint)
	}
	if c.TunnelPrefix != DefaultTunnelPrefix || c.PortalTunnelAddress() != netip.MustParseAddr("100.64.42.1") {
		t.Errorf("tunnel = %s / %s", c.TunnelPrefix, c.PortalTunnelAddress())
	}
	if !c.SecureCookies() {
		t.Error("cookies should be Secure for an https URL")
	}
}

func TestLoadConfigRejects(t *testing.T) {
	for name, m := range map[string]map[string]string{
		"missing URL":     {},
		"bad scheme":      {"EZDR_PUBLIC_URL": "ftp://x"},
		"bad TLS mode":    {"EZDR_PUBLIC_URL": "https://x", "EZDR_TLS": "maybe"},
		"pin+self-signed": {"EZDR_PUBLIC_URL": "https://x", "EZDR_TLS": "self-signed", "EZDR_TLS_PIN": "p"},
		"tiny prefix":     {"EZDR_PUBLIC_URL": "https://x", "EZDR_TUNNEL_PREFIX": "100.64.42.0/30"},
		"unmasked prefix": {"EZDR_PUBLIC_URL": "https://x", "EZDR_TUNNEL_PREFIX": "100.64.42.5/28"},
		"ipv6 prefix":     {"EZDR_PUBLIC_URL": "https://x", "EZDR_TUNNEL_PREFIX": "fd00::/64"},
		"bad port":        {"EZDR_PUBLIC_URL": "https://x", "EZDR_WG_PORT": "70000"},
		"site overlap":    {"EZDR_PUBLIC_URL": "https://x", "EZDR_SITE_TUNNEL_PREFIX": "100.64.42.0/28"},
	} {
		if _, err := LoadConfig(env(m)); err == nil {
			t.Errorf("%s: LoadConfig succeeded", name)
		}
	}
}

func TestTrustedProxyDefaults(t *testing.T) {
	behindProxy, err := LoadConfig(env(map[string]string{"EZDR_PUBLIC_URL": "https://x"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(behindProxy.TrustedProxies) == 0 {
		t.Error("behind a proxy, private ranges should be trusted by default")
	}
	edge, err := LoadConfig(env(map[string]string{"EZDR_PUBLIC_URL": "https://x", "EZDR_TLS": "self-signed"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(edge.TrustedProxies) != 0 {
		t.Errorf("self-signed mode should trust no proxies by default, got %v", edge.TrustedProxies)
	}
	none, _ := LoadConfig(env(map[string]string{"EZDR_PUBLIC_URL": "https://x", "EZDR_TRUSTED_PROXIES": "none"}))
	if len(none.TrustedProxies) != 0 {
		t.Error(`EZDR_TRUSTED_PROXIES=none should trust no proxies`)
	}
}
