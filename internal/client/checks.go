package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// Check is a prerequisite check result.
type Check struct {
	Name string
	Err  error
	// Warning marks a problem that does not block enrollment.
	Warning bool
}

// RunChecks runs the enrollment prerequisite checks for the given tunnel
// range. force skips the "already enrolled" check.
func RunChecks(ctx context.Context, tunnelPrefix netip.Prefix, force bool) []Check {
	checks := []Check{{Name: "running as root"}}
	if os.Geteuid() != 0 {
		checks[0].Err = errors.New("ezdr enroll must be run as root")
	}

	pve := Check{Name: fmt.Sprintf("Proxmox VE %d.x", SupportedPVEMajor)}
	if v, err := PVEVersion(ctx); err != nil {
		pve.Err = err
	} else if major, err := PVEMajor(v); err != nil {
		pve.Err = err
	} else if major != SupportedPVEMajor {
		pve.Err = fmt.Errorf("found Proxmox VE %d; only %d.x is supported", major, SupportedPVEMajor)
	}
	checks = append(checks, pve)

	wg := Check{Name: "WireGuard kernel module"}
	if err := run(ctx, "modprobe", "wireguard"); err != nil {
		wg.Err = fmt.Errorf("cannot load the wireguard module: %w", err)
	}
	checks = append(checks, wg)

	clock := Check{Name: "system clock synchronized", Warning: true}
	if out, err := exec.CommandContext(ctx, "timedatectl", "show", "-p", "NTPSynchronized", "--value").Output(); err != nil {
		clock.Err = fmt.Errorf("could not check time synchronization: %w", err)
	} else if strings.TrimSpace(string(out)) != "yes" {
		clock.Err = errors.New("the clock is not NTP-synchronized; TOTP and TLS may fail")
	}
	checks = append(checks, clock)

	overlap := Check{Name: "tunnel range " + tunnelPrefix.String() + " is free"}
	if err := checkOverlap(tunnelPrefix); err != nil {
		overlap.Err = err
	}
	checks = append(checks, overlap)

	enrolled := Check{Name: "not already enrolled"}
	if _, err := os.Stat(ConfigFile); err == nil && !force {
		enrolled.Err = errors.New("this host is already enrolled; run `ezdr unenroll` first or pass --force")
	}
	checks = append(checks, enrolled)
	return checks
}

// checkOverlap reports whether the tunnel range overlaps any address or
// non-default route on the host, in any routing table. The client's own
// interface is ignored so re-enrolling does not conflict with itself.
func checkOverlap(p netip.Prefix) error {
	var existing []netip.Prefix
	own := -1
	if link, err := netlink.LinkByName(InterfaceName); err == nil {
		own = link.Attrs().Index
	}

	addrs, err := netlink.AddrList(nil, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("list addresses: %w", err)
	}
	for _, a := range addrs {
		if a.LinkIndex == own {
			continue
		}
		if pf, ok := prefixFromIPNet(a.IP, a.Mask); ok {
			existing = append(existing, pf)
		}
	}

	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4,
		&netlink.Route{Table: unix.RT_TABLE_UNSPEC}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return fmt.Errorf("list routes: %w", err)
	}
	for _, r := range routes {
		if r.Dst == nil || r.LinkIndex == own {
			continue // default route or our own interface
		}
		if pf, ok := prefixFromIPNet(r.Dst.IP, r.Dst.Mask); ok {
			existing = append(existing, pf)
		}
	}
	return firstOverlap(p, existing)
}

func firstOverlap(p netip.Prefix, existing []netip.Prefix) error {
	for _, e := range existing {
		if e.Bits() > 0 && e.Overlaps(p) {
			return fmt.Errorf("overlaps %s already in use on this host; choose another EZDR_TUNNEL_PREFIX in the portal", e)
		}
	}
	return nil
}

func prefixFromIPNet(ip net.IP, mask net.IPMask) (netip.Prefix, bool) {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return netip.Prefix{}, false
	}
	ones, _ := mask.Size()
	return netip.PrefixFrom(addr.Unmap(), ones).Masked(), true
}

func run(ctx context.Context, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput() //nolint:gosec // callers pass fixed commands
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}
