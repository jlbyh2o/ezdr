package client

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// tunnelMTU matches the portal's MTU.
const tunnelMTU = 1420

// GenerateKey creates a WireGuard private key and writes it to path,
// readable only by root.
func GenerateKey(path string) (wgtypes.Key, error) {
	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		return wgtypes.Key{}, err
	}
	if err := os.MkdirAll(ConfigDir, 0o700); err != nil {
		return wgtypes.Key{}, err
	}
	if err := writeFileAtomic(path, []byte(key.String()+"\n"), 0o600); err != nil {
		return wgtypes.Key{}, err
	}
	return key, nil
}

// LoadKey reads the WireGuard private key.
func LoadKey(path string) (wgtypes.Key, error) {
	b, err := os.ReadFile(path) //nolint:gosec // fixed path
	if err != nil {
		return wgtypes.Key{}, err
	}
	return wgtypes.ParseKey(strings.TrimSpace(string(b)))
}

// EnsureInterface creates (or updates) the ezdr0 WireGuard interface for c.
// It is idempotent and resolves the portal endpoint each time it runs.
func EnsureInterface(c Config, key wgtypes.Key) error {
	link, err := netlink.LinkByName(InterfaceName)
	var notFound netlink.LinkNotFoundError
	if errors.As(err, &notFound) {
		attrs := netlink.NewLinkAttrs()
		attrs.Name = InterfaceName
		attrs.MTU = tunnelMTU
		if err := netlink.LinkAdd(&netlink.Wireguard{LinkAttrs: attrs}); err != nil {
			return fmt.Errorf("create %s: %w", InterfaceName, err)
		}
		link, err = netlink.LinkByName(InterfaceName)
	}
	if err != nil {
		return fmt.Errorf("find %s: %w", InterfaceName, err)
	}
	if link.Type() != "wireguard" {
		return fmt.Errorf("%s exists but is not a WireGuard interface", InterfaceName)
	}

	endpoint, err := net.ResolveUDPAddr("udp4", c.WireGuardEndpoint)
	if err != nil {
		return fmt.Errorf("resolve portal endpoint %s: %w", c.WireGuardEndpoint, err)
	}
	portalKey, err := wgtypes.NewKey(c.PortalWireGuardPublicKey)
	if err != nil {
		return fmt.Errorf("portal public key: %w", err)
	}
	keepalive := time.Duration(c.PersistentKeepaliveSeconds) * time.Second

	wg, err := wgctrl.New()
	if err != nil {
		return err
	}
	defer func() { _ = wg.Close() }()
	err = wg.ConfigureDevice(InterfaceName, wgtypes.Config{
		PrivateKey:   &key,
		ReplacePeers: true,
		Peers: []wgtypes.PeerConfig{{
			PublicKey:                   portalKey,
			Endpoint:                    endpoint,
			PersistentKeepaliveInterval: &keepalive,
			ReplaceAllowedIPs:           true,
			AllowedIPs:                  []net.IPNet{hostNet(c.PortalTunnelAddress)},
		}},
	})
	if err != nil {
		return fmt.Errorf("configure %s: %w", InterfaceName, err)
	}

	// The host's address and a route to only the portal's address.
	addr := &netlink.Addr{IPNet: ptr(hostNet(c.TunnelAddress))}
	if err := netlink.AddrReplace(link, addr); err != nil {
		return fmt.Errorf("set %s address: %w", InterfaceName, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("bring up %s: %w", InterfaceName, err)
	}
	route := &netlink.Route{LinkIndex: link.Attrs().Index, Dst: ptr(hostNet(c.PortalTunnelAddress)), Scope: netlink.SCOPE_LINK}
	if err := netlink.RouteReplace(route); err != nil {
		return fmt.Errorf("add route to portal: %w", err)
	}
	return nil
}

// DeleteInterface removes ezdr0 if it exists.
func DeleteInterface() error {
	link, err := netlink.LinkByName(InterfaceName)
	var notFound netlink.LinkNotFoundError
	if errors.As(err, &notFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return netlink.LinkDel(link)
}

// LastHandshake returns the time of the last handshake with the portal, or
// the zero time if none has happened.
func LastHandshake() (time.Time, error) {
	wg, err := wgctrl.New()
	if err != nil {
		return time.Time{}, err
	}
	defer func() { _ = wg.Close() }()
	dev, err := wg.Device(InterfaceName)
	if err != nil {
		return time.Time{}, err
	}
	if len(dev.Peers) == 0 {
		return time.Time{}, nil
	}
	return dev.Peers[0].LastHandshakeTime, nil
}

func hostNet(a netip.Addr) net.IPNet {
	return net.IPNet{IP: a.AsSlice(), Mask: net.CIDRMask(a.BitLen(), a.BitLen())}
}

func ptr[T any](v T) *T { return &v }
