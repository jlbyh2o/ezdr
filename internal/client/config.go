// Package client implements the ezdr client that runs on Proxmox VE hosts.
package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
)

// Paths used by the client.
const (
	ConfigDir      = "/etc/ezdr"
	ConfigFile     = ConfigDir + "/config.json"
	PrivateKeyFile = ConfigDir + "/wireguard.key"
	InterfaceName  = "ezdr0"
	ServiceName    = "ezdr.service"
	// PackagedUnitFile is installed by the .deb package.
	PackagedUnitFile = "/usr/lib/systemd/system/" + ServiceName
	// LocalUnitFile is written by `ezdr enroll` when the client was installed
	// without the package.
	LocalUnitFile = "/etc/systemd/system/" + ServiceName
)

// Config is the client's enrollment, written by `ezdr enroll`.
type Config struct {
	HostID                     string       `json:"host_id"`
	PortalURL                  string       `json:"portal_url"`
	TLSPin                     string       `json:"tls_pin,omitempty"`
	TunnelAddress              netip.Addr   `json:"tunnel_address"`
	TunnelPrefix               netip.Prefix `json:"tunnel_prefix"`
	PortalTunnelAddress        netip.Addr   `json:"portal_tunnel_address"`
	PortalWireGuardPublicKey   []byte       `json:"portal_wireguard_public_key"`
	WireGuardEndpoint          string       `json:"wireguard_endpoint"`
	ClientAPIURL               string       `json:"client_api_url"`
	PersistentKeepaliveSeconds uint32       `json:"persistent_keepalive_seconds"`
	// InstalledUnit is set when enroll wrote LocalUnitFile, so unenroll
	// removes it.
	InstalledUnit bool `json:"installed_unit,omitempty"`
	// BootGuardTimeoutSeconds is how long Proxmox's autostart waits for the
	// portal at boot (0: DefaultBootGuardTimeout). Set it by editing this
	// file.
	BootGuardTimeoutSeconds uint32 `json:"boot_guard_timeout_seconds,omitempty"`
}

// ErrNotEnrolled is returned when the host has no enrollment.
var ErrNotEnrolled = errors.New("this host is not enrolled; run `ezdr enroll <token>`")

// LoadConfig reads the client configuration from dir.
func LoadConfig(dir string) (Config, error) {
	b, err := os.ReadFile(filepath.Join(dir, filepath.Base(ConfigFile))) //nolint:gosec // fixed path
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, ErrNotEnrolled
	}
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", ConfigFile, err)
	}
	return c, nil
}

// Save writes the configuration to dir, readable only by root.
func (c Config) Save(dir string) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, filepath.Base(ConfigFile)), append(b, '\n'), 0o600)
}

// writeFileAtomic writes data to a temporary file and renames it into place.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
