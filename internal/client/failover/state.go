package failover

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// HostState is the failover state of this host's guests (see
// docs/design/uninstall.md).
type HostState struct {
	// Locked are guests locked because their plan is failed over (on a
	// primary).
	Locked []uint32
	// FailedOver are guests a failover registered on this host (on a DR
	// host).
	FailedOver []uint32
}

// State scans this host's guest configurations.
func (r *Runner) State() (HostState, error) {
	var st HostState
	for _, typ := range []string{"qemu", "lxc"} {
		confs, err := filepath.Glob(filepath.Join(filepath.Dir(r.configPath(typ, 0)), "*.conf"))
		if err != nil {
			return st, err
		}
		for _, c := range confs {
			id, err := strconv.ParseUint(strings.TrimSuffix(filepath.Base(c), ".conf"), 10, 32)
			if err != nil {
				continue
			}
			b, err := os.ReadFile(c) //nolint:gosec // Proxmox guest configurations
			if err != nil {
				return st, err
			}
			conf := string(b)
			switch {
			case Locked(conf):
				st.Locked = append(st.Locked, uint32(id))
			case strings.Contains(description(conf), marker("")):
				st.FailedOver = append(st.FailedOver, uint32(id))
			}
		}
	}
	slices.Sort(st.Locked)
	slices.Sort(st.FailedOver)
	return st, nil
}
