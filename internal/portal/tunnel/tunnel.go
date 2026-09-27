// Package tunnel runs the portal's WireGuard endpoint in userspace.
//
// WireGuard and its TCP/IP stack run inside the portal process
// (wireguard-go with gVisor's netstack), so the portal needs no kernel
// module or extra container privileges, only a published UDP port. The
// network stack does not forward packets, so peers cannot reach each other
// through the portal.
package tunnel

import (
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// MTU leaves room for WireGuard overhead on typical 1500-byte links.
const MTU = 1420

// Tunnel is the portal's WireGuard interface.
type Tunnel struct {
	mu        sync.Mutex
	dev       *device.Device
	net       *netstack.Net
	addr      netip.Addr
	publicKey wgtypes.Key
}

// GeneratePrivateKey returns a new WireGuard private key.
func GeneratePrivateKey() ([]byte, error) {
	k, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		return nil, err
	}
	return k[:], nil
}

// Start brings up the tunnel with the given private key, tunnel address,
// and UDP listen port.
func Start(privateKey []byte, addr netip.Addr, port uint16) (*Tunnel, error) {
	key, err := wgtypes.NewKey(privateKey)
	if err != nil {
		return nil, fmt.Errorf("invalid WireGuard private key: %w", err)
	}
	tunDev, tnet, err := netstack.CreateNetTUN([]netip.Addr{addr}, nil, MTU)
	if err != nil {
		return nil, fmt.Errorf("create userspace network stack: %w", err)
	}
	logger := &device.Logger{
		Verbosef: func(string, ...any) {},
		Errorf: func(format string, args ...any) {
			slog.Warn("wireguard: " + fmt.Sprintf(format, args...))
		},
	}
	dev := device.NewDevice(tunDev, conn.NewDefaultBind(), logger)
	cfg := fmt.Sprintf("private_key=%s\nlisten_port=%d\n", hex.EncodeToString(key[:]), port)
	if err := dev.IpcSet(cfg); err != nil {
		dev.Close()
		return nil, fmt.Errorf("configure WireGuard: %w", err)
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		return nil, fmt.Errorf("start WireGuard: %w", err)
	}
	return &Tunnel{dev: dev, net: tnet, addr: addr, publicKey: key.PublicKey()}, nil
}

// PublicKey returns the portal's WireGuard public key.
func (t *Tunnel) PublicKey() []byte {
	return t.publicKey[:]
}

// Listen opens a TCP listener on the portal's tunnel address. Only peers can
// connect to it.
func (t *Tunnel) Listen(port uint16) (net.Listener, error) {
	return t.net.ListenTCPAddrPort(netip.AddrPortFrom(t.addr, port))
}

// AddPeer allows a host's key to use exactly one tunnel address.
func (t *Tunnel) AddPeer(publicKey []byte, addr netip.Addr) error {
	if len(publicKey) != 32 {
		return fmt.Errorf("public key must be 32 bytes")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.dev.IpcSet(fmt.Sprintf("public_key=%s\nreplace_allowed_ips=true\nallowed_ip=%s\n",
		hex.EncodeToString(publicKey), netip.PrefixFrom(addr, addr.BitLen())))
}

// RemovePeer removes a host's key. Removing an unknown key is not an error.
func (t *Tunnel) RemovePeer(publicKey []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.dev.IpcSet(fmt.Sprintf("public_key=%s\nremove=true\n", hex.EncodeToString(publicKey)))
}

// PeerCount returns the number of configured peers.
func (t *Tunnel) PeerCount() (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	state, err := t.dev.IpcGet()
	if err != nil {
		return 0, err
	}
	return strings.Count(state, "public_key="), nil
}

// Close shuts the tunnel down.
func (t *Tunnel) Close() {
	t.dev.Close()
}
