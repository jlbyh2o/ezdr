package client

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"

	"github.com/jlbyh2o/ezdr/internal/client/pve"
	enrollv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/enroll/v1"
	"github.com/jlbyh2o/ezdr/internal/gen/ezdr/enroll/v1/enrollv1connect"
	"github.com/jlbyh2o/ezdr/internal/token"
)

// EnrollOptions configures Enroll.
type EnrollOptions struct {
	Token string
	// Yes skips the confirmation prompt.
	Yes bool
	// Force replaces an existing enrollment on this host.
	Force bool
	Out   io.Writer
}

// Enroll joins this host to a portal. See docs/design/enrollment.md.
func Enroll(ctx context.Context, opts EnrollOptions) error {
	out := opts.Out
	tok, err := token.Decode(opts.Token)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "Contacting portal %s...\n", tok.PortalURL)
	ec := enrollv1connect.NewEnrollmentServiceClient(PublicHTTPClient(tok.TLSPin), tok.PortalURL)
	check, err := ec.CheckToken(ctx, connect.NewRequest(&enrollv1.CheckTokenRequest{TokenId: tok.ID, Secret: tok.Secret}))
	if err != nil {
		return fmt.Errorf("check token: %w", describe(err))
	}
	prefix, err := netip.ParsePrefix(check.Msg.TunnelPrefix)
	if err != nil {
		return fmt.Errorf("portal sent an invalid tunnel range: %w", err)
	}

	fmt.Fprintln(out, "\nChecking prerequisites:")
	failed := false
	for _, c := range RunChecks(ctx, prefix, opts.Force) {
		switch {
		case c.Err == nil:
			fmt.Fprintf(out, "  ✓ %s\n", c.Name)
		case c.Warning:
			fmt.Fprintf(out, "  ! %s: %v\n", c.Name, c.Err)
		default:
			fmt.Fprintf(out, "  ✗ %s: %v\n", c.Name, c.Err)
			failed = true
		}
	}
	if failed {
		return errors.New("prerequisite checks failed; nothing was changed and the token is still valid")
	}

	facts, err := HostFacts(ctx)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "\nEnrolling %q will make these changes:\n", facts.Hostname)
	if opts.Force && fileExists(ConfigFile) {
		fmt.Fprintln(out, "  • Remove the existing EZDR enrollment on this host")
	}
	fmt.Fprintf(out, "  • Create WireGuard interface %s (address from %s) connected to %s\n",
		InterfaceName, prefix, check.Msg.WireguardEndpoint)
	fmt.Fprintf(out, "  • Write configuration and a private key to %s\n", ConfigDir)
	fmt.Fprintf(out, "  • Create a read-only Proxmox VE API user and token (%s, role %s) for inventory\n", pve.TokenID, pve.Role)
	fmt.Fprintf(out, "  • Enable and start the %s systemd service\n", ServiceName)
	if !opts.Yes {
		ok, err := confirm(out, "\nContinue? [y/N] ")
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("canceled; nothing was changed")
		}
	}

	if opts.Force {
		if err := removeLocal(ctx); err != nil {
			return fmt.Errorf("remove existing enrollment: %w", err)
		}
	}

	key, err := GenerateKey(PrivateKeyFile)
	if err != nil {
		return fmt.Errorf("generate WireGuard key: %w", err)
	}
	pub := key.PublicKey()
	resp, err := ec.Enroll(ctx, connect.NewRequest(&enrollv1.EnrollRequest{
		TokenId: tok.ID, Secret: tok.Secret, WireguardPublicKey: pub[:], Facts: facts,
	}))
	if err != nil {
		_ = os.RemoveAll(ConfigDir)
		return fmt.Errorf("enroll: %w", describe(err))
	}

	m := resp.Msg
	cfg := Config{
		HostID: m.HostId, PortalURL: tok.PortalURL, TLSPin: tok.TLSPin,
		PortalWireGuardPublicKey: m.PortalWireguardPublicKey, WireGuardEndpoint: m.WireguardEndpoint,
		ClientAPIURL: m.ClientApiUrl, PersistentKeepaliveSeconds: m.PersistentKeepaliveSeconds,
	}
	if cfg.TunnelAddress, err = netip.ParseAddr(m.TunnelAddress); err != nil {
		return localSetupFailed("parse tunnel address", err)
	}
	if cfg.PortalTunnelAddress, err = netip.ParseAddr(m.PortalTunnelAddress); err != nil {
		return localSetupFailed("parse portal address", err)
	}
	if cfg.TunnelPrefix, err = netip.ParsePrefix(m.TunnelPrefix); err != nil {
		return localSetupFailed("parse tunnel range", err)
	}
	if cfg.InstalledUnit, err = InstallUnit(exe); err != nil {
		return localSetupFailed("install systemd unit", err)
	}
	if err := cfg.Save(ConfigDir); err != nil {
		return localSetupFailed("save configuration", err)
	}
	if _, err := pve.EnsureToken(ctx); err != nil {
		// Not fatal: the service retries, and enrollment is otherwise complete.
		fmt.Fprintf(out, "Warning: could not create the Proxmox VE API token (%v); the service will retry.\n", err)
	}
	if err := EnsureInterface(cfg, key); err != nil {
		return localSetupFailed("set up WireGuard", err)
	}
	if err := EnableService(ctx); err != nil {
		return localSetupFailed("start service", err)
	}
	if err := RestartService(ctx); err != nil {
		return localSetupFailed("start service", err)
	}

	fmt.Fprintf(out, "\nEnrolled as %s (tunnel address %s). Waiting for the tunnel...\n", facts.Hostname, cfg.TunnelAddress)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if hs, err := LastHandshake(); err == nil && !hs.IsZero() {
			fmt.Fprintln(out, "Tunnel is up. This host should now show as online in the portal.")
			return nil
		}
		time.Sleep(time.Second)
	}
	fmt.Fprintf(out, "Warning: no WireGuard handshake with the portal after 30 seconds.\n"+
		"Check that outbound UDP to %s is allowed. The service keeps retrying.\n", cfg.WireGuardEndpoint)
	return nil
}

func localSetupFailed(step string, err error) error {
	return fmt.Errorf("%s: %w\nThe host was registered in the portal, but local setup failed. "+
		"Remove the host in the portal, then enroll again with a new token", step, err)
}

// describe turns a Connect error into a friendlier message.
func describe(err error) error {
	var ce *connect.Error
	if errors.As(err, &ce) {
		switch ce.Code() {
		case connect.CodeUnavailable:
			return fmt.Errorf("cannot reach the portal: %s", ce.Message())
		case connect.CodeUnknown, connect.CodeInternal:
			return fmt.Errorf("portal error: %s", ce.Message())
		default:
			return errors.New(ce.Message())
		}
	}
	return err
}

// confirm asks a yes/no question on the terminal. It reads from /dev/tty
// because the install script pipes the script itself into stdin.
func confirm(out io.Writer, prompt string) (bool, error) {
	tty, err := os.Open("/dev/tty")
	if err != nil {
		return false, errors.New("no terminal available to confirm; rerun with --yes to proceed without confirmation")
	}
	defer func() { _ = tty.Close() }()
	fmt.Fprint(out, prompt)
	line, err := bufio.NewReader(tty).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}
