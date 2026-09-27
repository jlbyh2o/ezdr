package client

import (
	"context"
	"os"
	"strings"
)

// unitFile is written when the client was installed without the package.
const unitFile = `[Unit]
Description=EZDR client
Documentation=https://github.com/jlbyh2o/ezdr
Wants=network-online.target
After=network-online.target

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
	unit := strings.Replace(unitFile, "/usr/bin/ezdr", binaryPath, 1)
	return true, writeFileAtomic(LocalUnitFile, []byte(unit), 0o644)
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
