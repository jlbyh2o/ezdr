package client

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"time"

	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
)

// Site tunnel (host-to-host replication) interface and key.
const (
	SiteInterfaceName = "ezdr1"
	SiteKeyFile       = ConfigDir + "/site-wireguard.key"
)

// EnsureSiteKey returns the site tunnel key, creating it if needed.
func EnsureSiteKey() (wgtypes.Key, error) {
	if k, err := LoadKey(SiteKeyFile); err == nil {
		return k, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return wgtypes.Key{}, err
	}
	return GenerateKey(SiteKeyFile)
}

// ApplySiteTunnel makes ezdr1 match t, or removes it when t is nil.
func ApplySiteTunnel(t *clientv1.SiteTunnel, key wgtypes.Key) error {
	if t == nil {
		return deleteLink(SiteInterfaceName)
	}
	self, err := netip.ParseAddr(t.Address)
	if err != nil {
		return fmt.Errorf("site tunnel address: %w", err)
	}
	prefix, err := netip.ParsePrefix(t.Prefix)
	if err != nil || !prefix.Contains(self) {
		return fmt.Errorf("site tunnel range %q doesn't contain %s", t.Prefix, self)
	}
	if err := checkOverlap(prefix); err != nil {
		return fmt.Errorf("site tunnel range: %w", err)
	}

	link, err := netlink.LinkByName(SiteInterfaceName)
	var notFound netlink.LinkNotFoundError
	if errors.As(err, &notFound) {
		attrs := netlink.NewLinkAttrs()
		attrs.Name, attrs.MTU = SiteInterfaceName, tunnelMTU
		if err := netlink.LinkAdd(&netlink.Wireguard{LinkAttrs: attrs}); err != nil {
			return fmt.Errorf("create %s: %w", SiteInterfaceName, err)
		}
		link, err = netlink.LinkByName(SiteInterfaceName)
	}
	if err != nil {
		return err
	}
	if link.Type() != "wireguard" {
		return fmt.Errorf("%s exists but is not a WireGuard interface", SiteInterfaceName)
	}

	cfg := wgtypes.Config{PrivateKey: &key, ReplacePeers: true}
	if t.ListenPort > 0 {
		port := int(t.ListenPort)
		cfg.ListenPort = &port
	}
	wanted := map[netip.Addr]bool{}
	for _, p := range t.Peers {
		pk, err := wgtypes.NewKey(p.PublicKey)
		if err != nil {
			return fmt.Errorf("peer key: %w", err)
		}
		addr, err := netip.ParseAddr(p.Address)
		if err != nil || !prefix.Contains(addr) {
			return fmt.Errorf("peer address %q is outside %s", p.Address, prefix)
		}
		pc := wgtypes.PeerConfig{PublicKey: pk, ReplaceAllowedIPs: true, AllowedIPs: []net.IPNet{hostNet(addr)}}
		if p.Endpoint != "" {
			ep, err := net.ResolveUDPAddr("udp", p.Endpoint)
			if err != nil {
				return fmt.Errorf("resolve tunnel endpoint %s: %w", p.Endpoint, err)
			}
			pc.Endpoint = ep
		}
		if p.PersistentKeepaliveSeconds > 0 {
			ka := time.Duration(p.PersistentKeepaliveSeconds) * time.Second
			pc.PersistentKeepaliveInterval = &ka
		}
		cfg.Peers = append(cfg.Peers, pc)
		wanted[addr] = true
	}
	wg, err := wgctrl.New()
	if err != nil {
		return err
	}
	defer func() { _ = wg.Close() }()
	if err := wg.ConfigureDevice(SiteInterfaceName, cfg); err != nil {
		return fmt.Errorf("configure %s: %w", SiteInterfaceName, err)
	}

	if err := netlink.AddrReplace(link, &netlink.Addr{IPNet: ptr(hostNet(self))}); err != nil {
		return fmt.Errorf("set %s address: %w", SiteInterfaceName, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("bring up %s: %w", SiteInterfaceName, err)
	}
	// One route per peer; remove routes to peers that are gone.
	routes, err := netlink.RouteList(link, netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	for _, r := range routes {
		if r.Dst == nil {
			continue
		}
		a, ok := netip.AddrFromSlice(r.Dst.IP)
		if ones, _ := r.Dst.Mask.Size(); ok && ones == 32 && !wanted[a.Unmap()] {
			_ = netlink.RouteDel(&r)
		}
	}
	for addr := range wanted {
		r := &netlink.Route{LinkIndex: link.Attrs().Index, Dst: ptr(hostNet(addr)), Scope: netlink.SCOPE_LINK}
		if err := netlink.RouteReplace(r); err != nil {
			return fmt.Errorf("route to %s: %w", addr, err)
		}
	}
	return nil
}

func deleteLink(name string) error {
	link, err := netlink.LinkByName(name)
	var notFound netlink.LinkNotFoundError
	if errors.As(err, &notFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return netlink.LinkDel(link)
}
