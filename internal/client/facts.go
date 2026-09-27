package client

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	enrollv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/enroll/v1"
	"github.com/jlbyh2o/ezdr/internal/version"
)

// SupportedPVEMajor is the Proxmox VE major version this client supports.
const SupportedPVEMajor = 9

// HostFacts gathers the facts sent to the portal at enrollment.
func HostFacts(ctx context.Context) (*enrollv1.HostFacts, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return nil, err
	}
	machineID, err := os.ReadFile("/etc/machine-id")
	if err != nil {
		return nil, fmt.Errorf("read machine ID: %w", err)
	}
	pve, err := PVEVersion(ctx)
	if err != nil {
		return nil, err
	}
	return &enrollv1.HostFacts{
		Hostname:      hostname,
		MachineId:     strings.TrimSpace(string(machineID)),
		PveVersion:    pve,
		ClientVersion: version.Version,
	}, nil
}

// PVEVersion returns the first line of `pveversion`, for example
// "pve-manager/9.2.20/49318c671b82f31e (running kernel: ...)".
func PVEVersion(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "pveversion").Output()
	if err != nil {
		return "", fmt.Errorf("run pveversion (is this a Proxmox VE host?): %w", err)
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return line, nil
}

// PVEMajor extracts the major version from `pveversion` output.
func PVEMajor(pveversion string) (int, error) {
	_, rest, ok := strings.Cut(pveversion, "pve-manager/")
	if !ok {
		return 0, fmt.Errorf("unrecognized pveversion output %q", pveversion)
	}
	major, _, _ := strings.Cut(rest, ".")
	return strconv.Atoi(major)
}
