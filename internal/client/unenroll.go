package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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
	fmt.Fprintln(out, "Removed the EZDR service, the "+InterfaceName+" interface, and "+ConfigDir+".")
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
	if cfg.InstalledUnit {
		if err := os.Remove(LocalUnitFile); err != nil && !os.IsNotExist(err) {
			return err
		}
		_ = run(ctx, "systemctl", "daemon-reload")
	}
	return os.RemoveAll(ConfigDir)
}
