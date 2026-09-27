package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/jlbyh2o/ezdr/internal/client/pve"
	"github.com/jlbyh2o/ezdr/internal/client/zrepl"
	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
)

// Unenroll removes EZDR's service, interface, and configuration from this
// host. The host must also be removed in the portal.
func Unenroll(ctx context.Context, out io.Writer) error {
	if !fileExists(ConfigFile) {
		return ErrNotEnrolled
	}
	if err := removeLocal(ctx); err != nil {
		return err
	}
	fmt.Fprintln(out, "Removed the EZDR service, the "+InterfaceName+" interface, the "+pve.User+" API user, and "+ConfigDir+".")
	fmt.Fprintln(out, "EZDR's zrepl jobs were removed; zrepl, replicas, and snapshots were left in place.")
	fmt.Fprintln(out, "Remove this host in the portal as well, if you haven't already.")
	return nil
}

// removeLocal removes everything enroll created on this host.
func removeLocal(ctx context.Context) error {
	cfg, err := LoadConfig(ConfigDir)
	if err != nil && !errors.Is(err, ErrNotEnrolled) {
		return err
	}
	if err := DisableService(ctx); err != nil {
		return fmt.Errorf("stop service: %w", err)
	}
	if err := DeleteInterface(); err != nil {
		return fmt.Errorf("remove %s: %w", InterfaceName, err)
	}
	if err := deleteLink(SiteInterfaceName); err != nil {
		return fmt.Errorf("remove %s: %w", SiteInterfaceName, err)
	}
	// Remove EZDR's zrepl jobs. zrepl itself, replicas, and snapshots stay.
	if err := zrepl.NewApplier().Apply(ctx, &clientv1.Zrepl{}); err != nil {
		return fmt.Errorf("remove EZDR's zrepl jobs: %w", err)
	}
	if err := pve.RemoveToken(ctx); err != nil {
		return fmt.Errorf("remove Proxmox VE API user %s: %w", pve.User, err)
	}
	if cfg.InstalledUnit {
		if err := os.Remove(LocalUnitFile); err != nil && !os.IsNotExist(err) {
			return err
		}
		_ = run(ctx, "systemctl", "daemon-reload")
	}
	return os.RemoveAll(ConfigDir)
}
