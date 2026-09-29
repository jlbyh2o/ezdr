package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jlbyh2o/ezdr/internal/client/failover"
	"github.com/jlbyh2o/ezdr/internal/client/pve"
	"github.com/jlbyh2o/ezdr/internal/client/testfailover"
	"github.com/jlbyh2o/ezdr/internal/client/zrepl"
	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
)

// RemoveOptions are the options of `ezdr unenroll` and `ezdr uninstall`
// (docs/design/uninstall.md).
type RemoveOptions struct {
	// Yes skips the confirmation.
	Yes bool
	// Force proceeds in unsafe states: locked guests are unlocked and test
	// failovers' guests removed.
	Force bool
	// Package is set when the package's pre-removal script runs the
	// cleanup: no questions, and unsafe states abort the removal.
	Package bool
	// Uninstall also purges the package.
	Uninstall bool
	Out       io.Writer
}

// Remove removes everything EZDR set up on this host, leaving Proxmox VE,
// the guests, and zrepl working without it. Replicas and snapshots stay.
func Remove(ctx context.Context, opts RemoveOptions) error {
	out := opts.Out
	fo, tests, zr := failover.NewRunner(), testfailover.NewRunner(), zrepl.NewApplier()
	st, err := fo.State()
	if err != nil {
		return fmt.Errorf("read guest configurations: %w", err)
	}
	markers, _ := fo.Markers()
	left, err := tests.FindLeftovers(ctx)
	if err != nil {
		return fmt.Errorf("look for test failovers: %w", err)
	}
	jobs, err := zr.EZDRJobs()
	if err != nil {
		return err
	}

	var unsafe []string
	if len(st.Locked) > 0 {
		unsafe = append(unsafe, fmt.Sprintf("guests %s are locked because their plan is failed over: fail it back first "+
			"(with --force, they're unlocked and start at boot again as before; don't if the DR copies still run)", ids(st.Locked)))
	}
	if len(st.FailedOver) > 0 || len(markers) > 0 {
		unsafe = append(unsafe, fmt.Sprintf("guests of a failed-over plan run on this host%s: fail it back first "+
			"(with --force, they keep running without EZDR, and failing back is no longer possible)", maybeIDs(st.FailedOver)))
	}
	if !left.Empty() {
		unsafe = append(unsafe, "a test failover's guests or clones are on this host: end the test first "+
			"(with --force, they're removed)")
	}
	if len(unsafe) > 0 && !opts.Force {
		cmd := "ezdr unenroll --force"
		if opts.Package || opts.Uninstall {
			cmd = "ezdr uninstall --force"
		}
		return fmt.Errorf("EZDR can't be removed safely now:\n  - %s\nTo remove it anyway, run `%s`", strings.Join(unsafe, "\n  - "), cmd)
	}

	fmt.Fprintln(out, "This removes EZDR from this host:")
	fmt.Fprintln(out, "  • the ezdr service, and its wait before Proxmox starts guests at boot")
	if len(jobs) > 0 {
		fmt.Fprintf(out, "  • EZDR's %d zrepl job(s), their holds and bookmarks, and their include in %s\n", len(jobs), zr.Paths.MainConfig)
	}
	for id := range left.Storages {
		fmt.Fprintf(out, "  • the test failover storage %s\n", id)
	}
	if opts.Force && len(unsafe) > 0 {
		for _, u := range unsafe {
			fmt.Fprintln(out, "  • "+u)
		}
	}
	fmt.Fprintf(out, "  • the %s and %s interfaces, the %s API user, %s, and %s\n", InterfaceName, SiteInterfaceName, pve.User, ConfigDir, stateDir)
	if opts.Uninstall {
		fmt.Fprintln(out, "  • the ezdr package")
	}
	fmt.Fprintln(out, "It keeps replicas and snapshots, zrepl and its other jobs, and guest configurations.")
	if !opts.Yes && !opts.Package {
		ok, err := confirm(out, "\nContinue? [y/N] ")
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("canceled; nothing was changed")
		}
	}

	if opts.Force {
		events, err := fo.Unlock(ctx, st.Locked, false)
		for _, e := range events {
			fmt.Fprintln(out, e)
		}
		if err != nil {
			return fmt.Errorf("unlock guests: %w", err)
		}
	}
	cfg, _ := LoadConfig(ConfigDir)
	if err := DisableService(ctx); err != nil {
		return fmt.Errorf("stop service: %w", err)
	}
	if err := RemoveBootGuard(ctx); err != nil {
		return fmt.Errorf("remove the boot guard: %w", err)
	}
	removal, err := zr.RemoveEZDR(ctx)
	if err != nil {
		return fmt.Errorf("remove EZDR from zrepl: %w", err)
	}
	done, err := tests.RemoveAll(ctx)
	for _, d := range done {
		fmt.Fprintln(out, d)
	}
	if err != nil {
		return fmt.Errorf("remove test failover data: %w", err)
	}
	if err := DeleteInterface(); err != nil {
		return fmt.Errorf("remove %s: %w", InterfaceName, err)
	}
	if err := deleteLink(SiteInterfaceName); err != nil {
		return fmt.Errorf("remove %s: %w", SiteInterfaceName, err)
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
	for _, dir := range []string{ConfigDir, stateDir, filepath.Dir(bootReady)} {
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
	}

	fmt.Fprintln(out, "\nEZDR was removed from this host.")
	if removal.Backup != "" {
		fmt.Fprintf(out, "zrepl runs without EZDR's jobs; its configuration before this change is in %s.\n", removal.Backup)
	}
	if backups, _ := filepath.Glob(zr.Paths.MainConfig + ".ezdr-takeover-*"); len(backups) > 0 {
		fmt.Fprintf(out, "Your zrepl configuration from before EZDR took it over is in %s.\n", strings.Join(backups, ", "))
	}
	fmt.Fprintln(out, "Replicas and snapshots were kept; delete them with zfs if you no longer need them.")
	fmt.Fprintln(out, "Remove this host in the portal as well, if you haven't already.")
	if opts.Uninstall {
		return purgePackage(ctx, out)
	}
	return nil
}

// stateDir holds what the client keeps between runs.
const stateDir = "/var/lib/ezdr"

// purgePackage removes the ezdr package. Its pre-removal script skips the
// cleanup, which just ran.
func purgePackage(ctx context.Context, out io.Writer) error {
	if err := exec.CommandContext(ctx, "dpkg-query", "-W", "ezdr").Run(); err != nil {
		fmt.Fprintln(out, "The ezdr package isn't installed; delete the ezdr binary to finish.")
		return nil
	}
	cmd := exec.CommandContext(ctx, "apt-get", "purge", "-y", "-qq", "ezdr")
	cmd.Env = append(os.Environ(), "EZDR_UNINSTALLING=1", "DEBIAN_FRONTEND=noninteractive")
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("remove the ezdr package: %w\n%s", err, b)
	}
	fmt.Fprintln(out, "The ezdr package was removed.")
	return nil
}

func ids(vmids []uint32) string {
	s := make([]string, len(vmids))
	for i, v := range vmids {
		s[i] = fmt.Sprint(v)
	}
	return strings.Join(s, ", ")
}

func maybeIDs(vmids []uint32) string {
	if len(vmids) == 0 {
		return ""
	}
	return " (" + ids(vmids) + ")"
}

// removeLocal removes what enroll created, before enrolling again.
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
