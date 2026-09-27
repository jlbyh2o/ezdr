package client

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// Status prints the host's enrollment and connection status.
func Status(ctx context.Context, out io.Writer) error {
	cfg, err := LoadConfig(ConfigDir)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Portal:          %s\n", cfg.PortalURL)
	fmt.Fprintf(out, "Host ID:         %s\n", cfg.HostID)
	fmt.Fprintf(out, "Tunnel address:  %s (portal %s)\n", cfg.TunnelAddress, cfg.PortalTunnelAddress)
	fmt.Fprintf(out, "WireGuard:       %s via %s\n", InterfaceName, cfg.WireGuardEndpoint)

	switch hs, err := LastHandshake(); {
	case err != nil:
		fmt.Fprintf(out, "Last handshake:  unavailable (%v)\n", err)
	case hs.IsZero():
		fmt.Fprintln(out, "Last handshake:  never")
	default:
		fmt.Fprintf(out, "Last handshake:  %s ago\n", time.Since(hs).Round(time.Second))
	}

	state, _ := exec.CommandContext(ctx, "systemctl", "is-active", ServiceName).Output()
	fmt.Fprintf(out, "Service:         %s\n", strings.TrimSpace(string(state)))
	return nil
}
