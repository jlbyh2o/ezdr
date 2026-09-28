// Package guests reads protected guests' Proxmox configurations on the
// primary and keeps the DR host's copies. See docs/design/test-failover.md.
package guests

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
)

// Paths are the directories the package reads and writes. Tests use a
// temporary root.
type Paths struct {
	// PVE is Proxmox's cluster file system, where qemu-server/ and lxc/ hold
	// this node's guest configurations.
	PVE string
	// Plans is where the DR host keeps plans' guest configurations.
	Plans string
}

// DefaultPaths are the real locations on a Proxmox VE host.
var DefaultPaths = Paths{PVE: "/etc/pve", Plans: "/var/lib/ezdr/plans"}

// Read returns a guest's current configuration: the configuration file
// without snapshot sections or pending changes, which start at the first
// "[section]" line.
func Read(p Paths, vmid uint32) (*clientv1.GuestConfig, error) {
	for _, typ := range []string{"qemu", "lxc"} {
		dir := "qemu-server"
		if typ == "lxc" {
			dir = "lxc"
		}
		b, err := os.ReadFile(filepath.Join(p.PVE, dir, strconv.FormatUint(uint64(vmid), 10)+".conf"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return &clientv1.GuestConfig{Vmid: vmid, Type: typ, Config: current(string(b))}, nil
	}
	return nil, fmt.Errorf("guest %d has no configuration on this host", vmid)
}

// current cuts a configuration at its first section header.
func current(conf string) string {
	var out []string
	for line := range strings.SplitSeq(conf, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "[") {
			break
		}
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n")) + "\n"
}

// ReadAll reads the configurations of vmids, skipping guests that no longer
// exist (they are reported as errors).
func ReadAll(p Paths, vmids []uint32) ([]*clientv1.GuestConfig, []error) {
	var out []*clientv1.GuestConfig
	var errs []error
	for _, id := range vmids {
		g, err := Read(p, id)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, g)
	}
	return out, errs
}

var planIDPattern = regexp.MustCompile(`^[a-z0-9]{1,64}$`)

// Meta describes a stored guest configuration.
type Meta struct {
	Type            string    `json:"type"`
	PlanName        string    `json:"plan_name"`
	PrimaryHostname string    `json:"primary_hostname"`
	ChangedAt       time.Time `json:"changed_at"`
}

// Store makes the DR host's stored configurations match plans: it writes
// each plan's guests and removes plans and guests that are no longer listed.
func Store(p Paths, plans []*clientv1.PlanGuestConfigs) error {
	keep := map[string]bool{}
	for _, pl := range plans {
		if !planIDPattern.MatchString(pl.PlanId) {
			return fmt.Errorf("invalid plan ID %q", pl.PlanId)
		}
		keep[pl.PlanId] = true
		dir := filepath.Join(p.Plans, pl.PlanId, "guests")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		want := map[string]bool{}
		for _, g := range pl.Guests {
			if g.Vmid == 0 || (g.Type != "qemu" && g.Type != "lxc") {
				return fmt.Errorf("plan %s: invalid guest %d (%q)", pl.PlanId, g.Vmid, g.Type)
			}
			id := strconv.FormatUint(uint64(g.Vmid), 10)
			want[id+".conf"], want[id+".json"] = true, true
			meta, err := json.Marshal(Meta{Type: g.Type, PlanName: pl.PlanName, PrimaryHostname: pl.PrimaryHostname,
				ChangedAt: g.GetChangedAt().AsTime()})
			if err != nil {
				return err
			}
			if err := writeIfChanged(filepath.Join(dir, id+".conf"), []byte(g.Config)); err != nil {
				return err
			}
			if err := writeIfChanged(filepath.Join(dir, id+".json"), meta); err != nil {
				return err
			}
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if !want[e.Name()] {
				if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
					return err
				}
			}
		}
	}
	entries, err := os.ReadDir(p.Plans)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() && planIDPattern.MatchString(e.Name()) && !keep[e.Name()] {
			if err := os.RemoveAll(filepath.Join(p.Plans, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// Stored is a guest configuration kept on the DR host.
type Stored struct {
	Vmid   uint32
	Config string
	Meta
}

// Load returns a plan's stored guest configurations.
func Load(p Paths, planID string) (map[uint32]Stored, error) {
	if !planIDPattern.MatchString(planID) {
		return nil, fmt.Errorf("invalid plan ID %q", planID)
	}
	dir := filepath.Join(p.Plans, planID, "guests")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return map[uint32]Stored{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[uint32]Stored{}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".conf")
		if !ok {
			continue
		}
		n, err := strconv.ParseUint(id, 10, 32)
		if err != nil {
			continue
		}
		conf, err := os.ReadFile(filepath.Join(dir, e.Name())) //nolint:gosec // our own directory
		if err != nil {
			return nil, err
		}
		s := Stored{Vmid: uint32(n), Config: string(conf)}
		if b, err := os.ReadFile(filepath.Join(dir, id+".json")); err == nil { //nolint:gosec // our own directory
			_ = json.Unmarshal(b, &s.Meta)
		}
		out[s.Vmid] = s
	}
	return out, nil
}

// writeIfChanged writes a file atomically unless it already has data.
func writeIfChanged(path string, data []byte) error {
	if old, err := os.ReadFile(path); err == nil && string(old) == string(data) { //nolint:gosec // our own directory
		return nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
