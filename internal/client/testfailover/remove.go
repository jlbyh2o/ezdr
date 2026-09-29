package testfailover

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Leftovers is what test failovers left on this host (see
// docs/design/uninstall.md): guests and clones exist only while a test
// runs, or if ending one failed.
type Leftovers struct {
	// Guests are test guests, by VMID, with their test ID and type.
	Guests []Leftover
	// Clones are datasets in the test storages.
	Clones []string
	// Storages maps test storages to their datasets.
	Storages map[string]string
}

// Leftover is a test guest.
type Leftover struct {
	TestID string
	Type   string
	VMID   uint32
}

var markerID = regexp.MustCompile(`ezdr-test-id-([a-z0-9]{1,40})`)

// FindLeftovers lists test guests, clones, and storages on this host.
func (r *Runner) FindLeftovers(ctx context.Context) (Leftovers, error) {
	l := Leftovers{Storages: map[string]string{}}
	for _, typ := range []string{"qemu", "lxc"} {
		confs, err := filepath.Glob(filepath.Join(filepath.Dir(r.configPath(typ, 0)), "*.conf"))
		if err != nil {
			return l, err
		}
		for _, c := range confs {
			id, err := strconv.ParseUint(strings.TrimSuffix(filepath.Base(c), ".conf"), 10, 32)
			if err != nil {
				continue
			}
			b, err := os.ReadFile(c) //nolint:gosec // Proxmox guest configurations
			if err != nil {
				return l, err
			}
			if m := markerID.FindStringSubmatch(description(string(b))); m != nil {
				l.Guests = append(l.Guests, Leftover{TestID: m[1], Type: typ, VMID: uint32(id)})
			}
		}
	}
	b, err := os.ReadFile(filepath.Join(r.PVE, "storage.cfg"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return l, err
	}
	id := ""
	for line := range strings.SplitSeq(string(b), "\n") {
		if typ, name, ok := strings.Cut(line, ":"); ok && line != "" && line[0] != ' ' && line[0] != '\t' {
			id = ""
			if name = strings.TrimSpace(name); strings.TrimSpace(typ) == "zfspool" && strings.HasPrefix(name, "ezdr-test-") {
				id = name
			}
			continue
		}
		if k, v, ok := strings.Cut(strings.TrimSpace(line), " "); ok && k == "pool" && id != "" {
			l.Storages[id] = strings.TrimSpace(v)
		}
	}
	for _, ds := range l.Storages {
		out, err := r.Run(ctx, "zfs", "list", "-H", "-o", "name", "-t", "filesystem,volume", "-r", ds)
		if err != nil {
			continue // the dataset is gone
		}
		for _, name := range strings.Fields(string(out)) {
			if name != ds {
				l.Clones = append(l.Clones, name)
			}
		}
	}
	slices.Sort(l.Clones)
	return l, nil
}

// Empty reports whether no test guests or clones are left.
func (l Leftovers) Empty() bool { return len(l.Guests) == 0 && len(l.Clones) == 0 }

// RemoveAll removes test guests (only those that pass the test's ownership
// checks), then the clones in the test storages, then the storages and
// their datasets. It returns what it did.
func (r *Runner) RemoveAll(ctx context.Context) ([]string, error) {
	l, err := r.FindLeftovers(ctx)
	if err != nil {
		return nil, err
	}
	var done []string
	for _, g := range l.Guests {
		if err := r.ours(g.TestID, g.Type, g.VMID); errors.Is(err, errGone) {
			continue
		} else if err != nil {
			return done, err
		}
		id := strconv.FormatUint(uint64(g.VMID), 10)
		if running, _ := r.running(ctx, g.Type, g.VMID); running {
			if _, err := r.Run(ctx, tool(g.Type), "stop", id); err != nil {
				return done, err
			}
		}
		args := []string{"destroy", id, "--purge", "1"}
		if g.Type == "qemu" {
			args = append(args, "--destroy-unreferenced-disks", "1")
		}
		if _, err := r.Run(ctx, tool(g.Type), args...); err != nil {
			return done, err
		}
		done = append(done, "destroyed test guest "+id)
	}
	// Datasets in a test storage are clones of replicas (guests' disks
	// that destroying the guests didn't remove).
	for _, ds := range l.Storages {
		out, err := r.Run(ctx, "zfs", "list", "-H", "-o", "name,origin", "-t", "filesystem,volume", "-r", ds)
		if err != nil {
			continue
		}
		for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
			name, origin, _ := strings.Cut(line, "\t")
			if name == ds || name == "" {
				continue
			}
			if origin == "-" || origin == "" {
				return done, fmt.Errorf("%s isn't a clone; not destroying it", name)
			}
			if _, err := r.Run(ctx, "zfs", "destroy", name); err != nil {
				return done, err
			}
			done = append(done, "destroyed clone "+name)
		}
	}
	for id, ds := range l.Storages {
		if _, err := r.Run(ctx, "pvesm", "remove", id); err != nil {
			return done, err
		}
		done = append(done, "removed storage "+id)
		if _, err := r.Run(ctx, "zfs", "list", "-H", "-o", "name", ds); err == nil {
			if _, err := r.Run(ctx, "zfs", "destroy", ds); err != nil {
				return done, err
			}
			done = append(done, "destroyed "+ds)
		}
	}
	return done, nil
}
