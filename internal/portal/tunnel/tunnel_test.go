package tunnel

import (
	"context"
	"encoding/hex"
	"io"
	"net/netip"
	"strings"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// TestPeersCanReachPortal starts a portal tunnel and a second userspace
// tunnel acting as a client, and checks that the client reaches the portal's
// listener only while it is a configured peer.
func TestPeersCanReachPortal(t *testing.T) {
	portalKey, _ := GeneratePrivateKey()
	portalAddr := netip.MustParseAddr("100.64.42.1")
	portal, err := Start(portalKey, portalAddr, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer portal.Close()
	ln, err := portal.Listen(8080)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = io.WriteString(c, "hello")
			_ = c.Close()
		}
	}()
	portalPort := listenPort(t, portal)

	clientKey, _ := wgtypes.GeneratePrivateKey()
	clientAddr := netip.MustParseAddr("100.64.42.2")
	client, err := Start(clientKey[:], clientAddr, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.AddPeer(portal.PublicKey(), portalAddr); err != nil {
		t.Fatal(err)
	}
	if err := client.dev.IpcSet("public_key=" + hex.EncodeToString(portal.PublicKey()) +
		"\nendpoint=127.0.0.1:" + portalPort + "\npersistent_keepalive_interval=1\n"); err != nil {
		t.Fatal(err)
	}

	dial := func() (string, error) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		c, err := client.net.DialContextTCPAddrPort(ctx, netip.AddrPortFrom(portalAddr, 8080))
		if err != nil {
			return "", err
		}
		defer func() { _ = c.Close() }()
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		b, err := io.ReadAll(c)
		return string(b), err
	}

	pub := clientKey.PublicKey()
	if err := portal.AddPeer(pub[:], clientAddr); err != nil {
		t.Fatal(err)
	}
	if got, err := dial(); err != nil || got != "hello" {
		t.Fatalf("dial as peer: %q, %v", got, err)
	}
	if n, _ := portal.PeerCount(); n != 1 {
		t.Errorf("PeerCount = %d, want 1", n)
	}

	if err := portal.RemovePeer(pub[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := dial(); err == nil {
		t.Fatal("removed peer could still reach the portal")
	}
}

func listenPort(t *testing.T, tun *Tunnel) string {
	t.Helper()
	state, err := tun.dev.IpcGet()
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(state, "\n") {
		if port, ok := strings.CutPrefix(line, "listen_port="); ok {
			return port
		}
	}
	t.Fatal("no listen port")
	return ""
}
