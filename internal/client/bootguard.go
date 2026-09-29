package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// The boot guard keeps Proxmox from starting a primary's guests at boot
// before the client has heard from the portal and locked the guests of
// failed-over plans (docs/design/failover.md, section 5.2).
const (
	// BootGuardDropIn hooks `ezdr boot-guard` into Proxmox's autostart.
	BootGuardDropIn = "/etc/systemd/system/pve-guests.service.d/ezdr-boot-guard.conf"
	// bootGuardState records whether this host is a plan's primary.
	bootGuardState = "/var/lib/ezdr/boot-guard.json"
	// bootReady exists once this boot's locks are applied (/run is emptied
	// at every boot).
	bootReady = "/run/ezdr/boot-ready"
	// DefaultBootGuardTimeout is how long the guard waits for the portal.
	DefaultBootGuardTimeout = 5 * time.Minute
)

// bootGuard holds the guard's files; tests replace them.
type bootGuard struct {
	dropIn, state, ready string
}

var defaultBootGuard = bootGuard{dropIn: BootGuardDropIn, state: bootGuardState, ready: bootReady}

type bootGuardFile struct {
	// Primary is set while this host is the primary of an active, paused,
	// or failed-over plan.
	Primary bool `json:"primary"`
}

func dropInContent(binary string) string {
	return "# Written by EZDR: before Proxmox starts guests at boot, wait until the EZDR\n" +
		"# client has locked any failed-over guests (at most a few minutes).\n" +
		"[Service]\nExecStartPre=-" + binary + " boot-guard\n"
}

// install writes the drop-in for binary. It reports whether it changed
// anything, in which case systemd must be reloaded.
func (g bootGuard) install(binary string) (bool, error) {
	want := dropInContent(binary)
	if b, err := os.ReadFile(g.dropIn); err == nil && string(b) == want {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(g.dropIn), 0o755); err != nil { //nolint:gosec // systemd directory
		return false, err
	}
	return true, writeFileAtomic(g.dropIn, []byte(want), 0o644)
}

// remove deletes the drop-in. It reports whether it existed.
func (g bootGuard) remove() (bool, error) {
	err := os.Remove(g.dropIn)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err == nil {
		_ = os.Remove(filepath.Dir(g.dropIn)) // only if empty
	}
	return err == nil, err
}

// setPrimary records whether this host is a plan's primary.
func (g bootGuard) setPrimary(primary bool) error {
	if cur, err := g.primary(); err == nil && cur == primary {
		return nil
	}
	b, _ := json.Marshal(bootGuardFile{Primary: primary})
	if err := os.MkdirAll(filepath.Dir(g.state), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(g.state, b, 0o600)
}

func (g bootGuard) primary() (bool, error) {
	b, err := os.ReadFile(g.state)
	if err != nil {
		return false, err
	}
	var f bootGuardFile
	return f.Primary, json.Unmarshal(b, &f)
}

// markReady records that this boot's locks are applied.
func (g bootGuard) markReady() error {
	if _, err := os.Stat(g.ready); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(g.ready), 0o700); err != nil {
		return err
	}
	return os.WriteFile(g.ready, nil, 0o600)
}

// wait blocks until the locks are applied or timeout passes. Hosts that
// aren't a plan's primary don't wait.
func (g bootGuard) wait(ctx context.Context, out io.Writer, timeout time.Duration, poll time.Duration) {
	if primary, err := g.primary(); err != nil || !primary {
		return
	}
	deadline := time.Now().Add(timeout)
	fmt.Fprintf(out, "waiting up to %s for the EZDR client to lock failed-over guests\n", timeout)
	for {
		if _, err := os.Stat(g.ready); err == nil {
			fmt.Fprintln(out, "the EZDR client applied the portal's locks; starting guests")
			return
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(out, "no answer from the portal within %s; starting guests as usual\n", timeout)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(poll):
		}
	}
}

// InstallBootGuard hooks the boot guard into Proxmox's autostart for this
// binary.
func InstallBootGuard(ctx context.Context) error {
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	if binary, err = filepath.EvalSymlinks(binary); err != nil {
		return err
	}
	changed, err := defaultBootGuard.install(binary)
	if err != nil || !changed {
		return err
	}
	return run(ctx, "systemctl", "daemon-reload")
}

// RemoveBootGuard removes the boot guard's hook.
func RemoveBootGuard(ctx context.Context) error {
	removed, err := defaultBootGuard.remove()
	if err != nil || !removed {
		return err
	}
	return run(ctx, "systemctl", "daemon-reload")
}

// BootGuard runs before Proxmox starts guests at boot. It never fails: an
// error only means guests start as they would without EZDR.
func BootGuard(ctx context.Context, out io.Writer) {
	timeout := DefaultBootGuardTimeout
	cfg, err := LoadConfig(ConfigDir)
	if err != nil {
		return // not enrolled
	}
	if cfg.BootGuardTimeoutSeconds > 0 {
		timeout = time.Duration(cfg.BootGuardTimeoutSeconds) * time.Second
	}
	defaultBootGuard.wait(ctx, out, timeout, time.Second)
}
