package failover

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
	"github.com/jlbyh2o/ezdr/internal/guestconfig"
)

// Cleanup removes a plan's failed-over guests from the DR host after the
// guests failed back (docs/design/failback.md, section 4): it deletes their
// configuration files, never their disks (which are the replicas), removes
// the plan's storages unless another guest uses one, rolls each replica
// back to the snapshot the primary has, and makes it read-only again. It
// also destroys the cloud-init drives the failover created. Repeating it is
// safe. It returns what it did.
func (r *Runner) Cleanup(ctx context.Context, planID string, vmids []uint32, snapshot string) ([]string, error) {
	if !snapshotPattern.MatchString(snapshot) {
		return nil, fmt.Errorf("invalid snapshot name %q", snapshot)
	}
	rec, stored, err := r.recovery(planID)
	if err != nil {
		return nil, err
	}
	for _, s := range rec.Storages {
		if !storagePattern.MatchString(s.StorageId) || !datasetPattern.MatchString(s.ReceiveDataset+"/"+s.SourceDataset) {
			return nil, fmt.Errorf("invalid recovery storage %v", s)
		}
	}

	var notes []string
	for _, vmid := range vmids {
		typ, _, err := r.guest(vmid)
		if errors.Is(err, os.ErrNotExist) {
			continue // removed by an earlier attempt
		}
		if err != nil {
			return notes, err
		}
		if err := r.oursOnDR(planID, typ, vmid); err != nil {
			return notes, err
		}
		if running, err := r.running(ctx, typ, vmid); err != nil {
			return notes, err
		} else if running {
			return notes, fmt.Errorf("guest %d is still running", vmid)
		}
		if err := os.Remove(r.configPath(typ, vmid)); err != nil {
			return notes, err
		}
		notes = append(notes, fmt.Sprintf("removed guest %d (configuration only)", vmid))
	}

	ids, err := r.storageIDs()
	if err != nil {
		return notes, err
	}
	for _, s := range rec.Storages {
		if !slices.Contains(ids, s.StorageId) {
			continue
		}
		users, err := r.storageUsers(s.StorageId)
		if err != nil {
			return notes, err
		}
		if len(users) > 0 {
			notes = append(notes, fmt.Sprintf("kept storage %s: guest(s) %s use it", s.StorageId, strings.Join(users, ", ")))
			continue
		}
		if _, err := r.Run(ctx, "pvesm", "remove", s.StorageId); err != nil {
			return notes, err
		}
		notes = append(notes, "removed storage "+s.StorageId)
	}

	for _, vmid := range vmids {
		st, ok := stored[vmid]
		if !ok {
			return notes, fmt.Errorf("guest %d: this host has no copy of its configuration", vmid)
		}
		for _, d := range guestconfig.Disks(st.Type, st.Config) {
			source, volname, _ := strings.Cut(d.Volume, ":")
			i := slices.IndexFunc(rec.Storages, func(s *clientv1.RecoveryStorage) bool { return s.SourceStorage == source })
			if i < 0 {
				continue
			}
			s := rec.Storages[i]
			ds := s.ReceiveDataset + "/" + s.SourceDataset + "/" + volname[strings.LastIndex(volname, "/")+1:]
			// Changes since the snapshot (none are expected: the guest was
			// stopped before it) would stop replication from resuming.
			if _, err := r.Run(ctx, "zfs", "rollback", ds+"@"+snapshot); err != nil {
				return notes, err
			}
			if _, err := r.Run(ctx, "zfs", "set", "readonly=on", ds); err != nil {
				return notes, err
			}
		}
		note, err := r.removeCloudInit(ctx, vmid, st.Config, rec.Storages)
		if err != nil {
			return notes, err
		}
		if note != "" {
			notes = append(notes, note)
		}
	}
	return notes, nil
}

// removeCloudInit destroys the cloud-init drive the failover created for a
// VM (see ensureCloudInit), which is left on the plan's storage once the
// guest is removed. A drive with snapshots is a replica from an earlier
// setup that the failover reused, and is kept.
func (r *Runner) removeCloudInit(ctx context.Context, vmid uint32, original string, storages []*clientv1.RecoveryStorage) (string, error) {
	if key, _ := cloudInitDrive(original); key == "" {
		return "", nil
	}
	name := "vm-" + strconv.FormatUint(uint64(vmid), 10) + "-cloudinit"
	for _, s := range storages {
		ds := s.ReceiveDataset + "/" + s.SourceDataset + "/" + name
		if _, err := r.Run(ctx, "zfs", "list", "-H", "-o", "name", ds); err != nil {
			continue // not there (or removed by an earlier attempt)
		}
		out, err := r.Run(ctx, "zfs", "list", "-H", "-t", "snapshot", "-o", "name", "-d", "1", ds)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(string(out)) != "" {
			continue
		}
		if _, err := r.Run(ctx, "zfs", "destroy", ds); err != nil {
			return "", err
		}
		return fmt.Sprintf("removed guest %d's cloud-init drive %s", vmid, ds), nil
	}
	return "", nil
}

// storageUsers lists the guests on this node whose configuration refers to
// a storage.
func (r *Runner) storageUsers(storage string) ([]string, error) {
	var users []string
	for _, dir := range []string{"qemu-server", "lxc"} {
		files, err := filepath.Glob(filepath.Join(r.PVE, dir, "*.conf"))
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			b, err := os.ReadFile(f) //nolint:gosec // Proxmox's guest configurations
			if err != nil {
				return nil, err
			}
			if strings.Contains(string(b), storage+":") {
				id := strings.TrimSuffix(filepath.Base(f), ".conf")
				if _, err := strconv.ParseUint(id, 10, 32); err == nil {
					users = append(users, id)
				}
			}
		}
	}
	return users, nil
}
