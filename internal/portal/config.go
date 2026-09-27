package portal

import (
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// TLS modes for the public listener.
const (
	// TLSOff serves plain HTTP, for use behind a reverse proxy such as Caddy.
	TLSOff = "off"
	// TLSSelfSigned serves HTTPS with a self-signed certificate that the
	// portal generates and keeps. Tokens automatically carry its TLS pin.
	TLSSelfSigned = "self-signed"
)

// Config is the portal configuration, read from environment variables.
type Config struct {
	// Listen is the public listener address (EZDR_LISTEN).
	Listen string
	// DataDir holds the database and generated keys (EZDR_DATA_DIR).
	DataDir string
	// PublicURL is the URL users and clients reach the portal at
	// (EZDR_PUBLIC_URL). Tokens can only be created when it uses HTTPS.
	PublicURL *url.URL
	// TLS is TLSOff or TLSSelfSigned (EZDR_TLS).
	TLS string
	// TLSPin is a manually configured pin embedded in tokens (EZDR_TLS_PIN).
	TLSPin string
	// WireGuardPort is the UDP port the portal's WireGuard listens on
	// (EZDR_WG_PORT).
	WireGuardPort uint16
	// WireGuardEndpoint is the host:port clients connect to
	// (EZDR_WG_ENDPOINT). It defaults to the public URL's host and
	// WireGuardPort.
	WireGuardEndpoint string
	// TunnelPrefix is the tunnel address range (EZDR_TUNNEL_PREFIX).
	TunnelPrefix netip.Prefix
	// SecretKeyFile holds the 32-byte key that encrypts secrets in the
	// database (EZDR_SECRET_KEY_FILE). It is generated if missing.
	SecretKeyFile string
	// TrustedProxies are the source ranges allowed to set X-Forwarded-For
	// (EZDR_TRUSTED_PROXIES, comma-separated, or "none"). Behind a proxy
	// (EZDR_TLS=off) the default is loopback and private ranges; in
	// self-signed mode the portal is the edge, so the default is none.
	TrustedProxies []netip.Prefix
}

// DefaultTunnelPrefix avoids the start of 100.64.0.0/10, which other overlay
// tools allocate from first.
var DefaultTunnelPrefix = netip.MustParsePrefix("100.64.42.0/28")

var defaultTrustedProxies = []string{
	"127.0.0.0/8", "::1/128", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7",
}

// LoadConfig reads the configuration from the environment.
func LoadConfig(getenv func(string) string) (Config, error) {
	get := func(key, def string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return def
	}

	c := Config{
		Listen:  get("EZDR_LISTEN", ":8080"),
		DataDir: get("EZDR_DATA_DIR", "data"),
		TLS:     get("EZDR_TLS", TLSOff),
		TLSPin:  get("EZDR_TLS_PIN", ""),
	}
	c.SecretKeyFile = get("EZDR_SECRET_KEY_FILE", filepath.Join(c.DataDir, "secret.key"))

	raw := get("EZDR_PUBLIC_URL", "")
	if raw == "" {
		return c, errors.New("EZDR_PUBLIC_URL is required (for example, https://portal.example.com)")
	}
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return c, fmt.Errorf("EZDR_PUBLIC_URL %q is not a valid http(s) URL", raw)
	}
	c.PublicURL = u

	switch c.TLS {
	case TLSOff, TLSSelfSigned:
	default:
		return c, fmt.Errorf("EZDR_TLS must be %q or %q", TLSOff, TLSSelfSigned)
	}
	if c.TLS == TLSSelfSigned && c.TLSPin != "" {
		return c, errors.New("EZDR_TLS_PIN cannot be combined with EZDR_TLS=self-signed, which sets the pin automatically")
	}

	port, err := strconv.ParseUint(get("EZDR_WG_PORT", "51820"), 10, 16)
	if err != nil || port == 0 {
		return c, errors.New("EZDR_WG_PORT must be a port number")
	}
	c.WireGuardPort = uint16(port)
	c.WireGuardEndpoint = get("EZDR_WG_ENDPOINT", net.JoinHostPort(u.Hostname(), strconv.Itoa(int(port))))
	if _, _, err := net.SplitHostPort(c.WireGuardEndpoint); err != nil {
		return c, fmt.Errorf("EZDR_WG_ENDPOINT %q must be host:port", c.WireGuardEndpoint)
	}

	c.TunnelPrefix = DefaultTunnelPrefix
	if v := get("EZDR_TUNNEL_PREFIX", ""); v != "" {
		p, err := netip.ParsePrefix(v)
		if err != nil || !p.Addr().Is4() || p.Bits() > 29 || p.Masked() != p {
			return c, fmt.Errorf("EZDR_TUNNEL_PREFIX %q must be an IPv4 network of /29 or larger, such as 100.64.42.0/28", v)
		}
		c.TunnelPrefix = p
	}

	defaultProxies := strings.Join(defaultTrustedProxies, ",")
	if c.TLS == TLSSelfSigned {
		defaultProxies = "none"
	}
	proxies := get("EZDR_TRUSTED_PROXIES", defaultProxies)
	if proxies == "none" {
		return c, nil
	}
	for _, p := range strings.Split(proxies, ",") {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(p))
		if err != nil {
			return c, fmt.Errorf("EZDR_TRUSTED_PROXIES: %w", err)
		}
		c.TrustedProxies = append(c.TrustedProxies, prefix)
	}
	return c, nil
}

// PortalTunnelAddress is the portal's own address: the first usable address
// in the tunnel range.
func (c Config) PortalTunnelAddress() netip.Addr {
	return c.TunnelPrefix.Addr().Next()
}

// SecureCookies reports whether session cookies should be marked Secure.
func (c Config) SecureCookies() bool {
	return c.PublicURL.Scheme == "https"
}

// LoadOrCreateSecretKey reads the secret key file, creating it with a random
// key if it does not exist.
func LoadOrCreateSecretKey(path string) ([]byte, error) {
	key, err := os.ReadFile(path) //nolint:gosec // path comes from configuration
	if err == nil {
		if len(key) != 32 {
			return nil, fmt.Errorf("secret key file %s must contain exactly 32 bytes", path)
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key = make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}
