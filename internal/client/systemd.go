package client

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// unitFile is written when the client was installed without the package.
const unitFile = `[Unit]
Description=EZDR client
Documentation=https://github.com/jlbyh2o/ezdr
Wants=network-online.target
# Reads guest configurations; Proxmox's autostart waits for it (boot guard),
# so it must not be ordered after pve-guests.service.
After=network-online.target pve-cluster.service

[Service]
ExecStart=/usr/bin/ezdr run
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
`

// InstallUnit writes the systemd unit unless the package already provides
// one. It reports whether it wrote the unit.
func InstallUnit(binaryPath string) (bool, error) {
	if fileExists(PackagedUnitFile) {
		return false, nil
	}
	// The service runs the binary as root: nobody else may be able to
	// replace it.
	if err := rootOnly(binaryPath); err != nil {
		return false, fmt.Errorf("%w; install ezdr in a root-owned directory such as /usr/local/bin first", err)
	}
	unit := strings.Replace(unitFile, "/usr/bin/ezdr", binaryPath, 1)
	return true, writeFileAtomic(LocalUnitFile, []byte(unit), 0o644)
}

// rootOnly checks that path and every directory above it are owned by root
// and not writable by anyone else.
func rootOnly(path string) error {
	p, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if strings.ContainsAny(p, "\n\r") {
		return fmt.Errorf("unsupported path %q", p)
	}
	for {
		fi, err := os.Stat(p)
		if err != nil {
			return err
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != 0 || fi.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("%s can be changed by users other than root", p)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return nil
		}
		p = parent
	}
}

// StartService reloads systemd, enables the service, and (re)starts it once,
// so a re-enrollment replaces any running instance.
func StartService(ctx context.Context) error {
	if err := run(ctx, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	if err := run(ctx, "systemctl", "enable", ServiceName); err != nil {
		return err
	}
	return run(ctx, "systemctl", "restart", ServiceName)
}

// DisableService stops and disables the service if a unit is installed.
func DisableService(ctx context.Context) error {
	if !fileExists(PackagedUnitFile) && !fileExists(LocalUnitFile) {
		return nil
	}
	return run(ctx, "systemctl", "disable", "--now", ServiceName)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
