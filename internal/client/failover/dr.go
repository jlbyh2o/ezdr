package failover

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jlbyh2o/ezdr/internal/client/guests"
	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
	"github.com/jlbyh2o/ezdr/internal/guestconfig"
)

// Tag marks guests registered on the DR host by a failover.
const Tag = "ezdr-failover"

var (
	datasetPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]*(/[A-Za-z0-9_.:-]+)+$`)
	snapshotPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]+$`)
	jobPattern      = regexp.MustCompile(`^ezdr_[A-Za-z0-9_-]{1,80}_pull$`)
	storagePattern  = regexp.MustCompile(`^ezdr-[a-z0-9-]+$`)
	sizePattern     = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?[KMGT]?$`)
)

// marker identifies a plan's failed-over guests in their description, with
// characters Proxmox never escapes.
func marker(planID string) string { return "ezdr-failover-" + planID }

// Snapshot takes a snapshot of each dataset (the final snapshot of a
// planned failover). Existing snapshots are kept.
func (r *Runner) Snapshot(ctx context.Context, datasets []string, snapshot string) error {
	if !snapshotPattern.MatchString(snapshot) {
		return fmt.Errorf("invalid snapshot name %q", snapshot)
	}
	for _, ds := range datasets {
		if !datasetPattern.MatchString(ds) {
			return fmt.Errorf("invalid dataset %q", ds)
		}
		if _, err := r.Run(ctx, "zfs", "list", "-H", "-o", "name", ds+"@"+snapshot); err == nil {
			continue
		}
		if _, err := r.Run(ctx, "zfs", "snapshot", ds+"@"+snapshot); err != nil {
			return err
		}
	}
	return nil
}

// Replicate starts the pull jobs now.
func (r *Runner) Replicate(ctx context.Context, jobs []string) error {
	for _, j := range jobs {
		if !jobPattern.MatchString(j) {
			return fmt.Errorf("invalid job %q", j)
		}
		if _, err := r.Run(ctx, "zrepl", "signal", "wakeup", j); err != nil {
			return err
		}
	}
	return nil
}

// recovery loads a plan's recovery information and guest configurations.
func (r *Runner) recovery(planID string) (*clientv1.PlanRecovery, map[uint32]guests.Stored, error) {
	all, err := guests.LoadRecovery(r.Guests)
	if err != nil {
		return nil, nil, err
	}
	for _, rec := range all {
		if rec.PlanId == planID {
			stored, err := guests.Load(r.Guests, planID)
			return rec, stored, err
		}
	}
	return nil, nil, fmt.Errorf("this host has no recovery information for plan %s", planID)
}

// Prepare makes the plan's replicas the guests' disks and registers the
// guests: for each disk it aborts a partial receive, makes the replica
// writable, and restores a container volume's size; then it adds the
// plan's Proxmox storages and registers each guest with its original ID.
// Guests this plan's failover already registered are skipped.
func (r *Runner) Prepare(ctx context.Context, planID string, vmids []uint32) ([]string, error) {
	rec, stored, err := r.recovery(planID)
	if err != nil {
		return nil, err
	}
	node, err := r.Node()
	if err != nil {
		return nil, err
	}
	storages := map[string]*clientv1.RecoveryStorage{}
	for _, s := range rec.Storages {
		if !storagePattern.MatchString(s.StorageId) || !datasetPattern.MatchString(s.ReceiveDataset+"/"+s.SourceDataset) {
			return nil, fmt.Errorf("invalid recovery storage %v", s)
		}
		storages[s.SourceStorage] = s
	}
	bridges := map[string]string{}
	for _, b := range rec.Bridges {
		bridges[b.SourceBridge] = b.TargetBridge
	}
	for _, s := range storages {
		if err := r.ensureStorage(ctx, s, node); err != nil {
			return nil, err
		}
	}
	pveStorages, err := r.storageIDs()
	if err != nil {
		return nil, err
	}
	used, err := r.usedVMIDs()
	if err != nil {
		return nil, err
	}

	var notes []string
	for _, vmid := range vmids {
		st, ok := stored[vmid]
		if !ok {
			return notes, fmt.Errorf("guest %d: this host has no copy of its configuration", vmid)
		}
		if used[vmid] {
			if err := r.oursOnDR(planID, st.Type, vmid); err != nil {
				return notes, fmt.Errorf("guest %d: ID in use: %w", vmid, err)
			}
			// Registered by an earlier attempt, which may have stopped
			// before the cloud-init drive.
			if err := r.ensureCloudInit(ctx, vmid, st.Config, storages); err != nil {
				return notes, fmt.Errorf("guest %d: %w", vmid, err)
			}
			continue
		}
		volumes := map[string]string{}
		for _, d := range guestconfig.Disks(st.Type, st.Config) {
			source, volname, _ := strings.Cut(d.Volume, ":")
			s := storages[source]
			if s == nil {
				return notes, fmt.Errorf("guest %d: %s isn't on a replicated storage", vmid, d.Volume)
			}
			name := volname[strings.LastIndex(volname, "/")+1:]
			ds := s.ReceiveDataset + "/" + s.SourceDataset + "/" + name
			if err := r.prepareReplica(ctx, ds, st.Type, d.Size); err != nil {
				return notes, fmt.Errorf("guest %d: %w", vmid, err)
			}
			volumes[d.Volume] = s.StorageId + ":" + name
		}
		res, err := guestconfig.Rewrite(st.Config, guestconfig.Mapping{
			Type: st.Type, Volumes: volumes, Bridges: bridges, Storages: pveStorages, KeepOnboot: true, Tag: Tag,
			Note: fmt.Sprintf("Failed over from %s by EZDR (plan %s, %s) at %s", st.PrimaryHostname, oneLine(rec.PlanName),
				marker(planID), time.Now().UTC().Format(time.RFC3339)),
			VMGenID: newUUID(),
		})
		if err != nil {
			return notes, fmt.Errorf("guest %d: %w", vmid, err)
		}
		if err := os.WriteFile(r.configPath(st.Type, vmid), []byte(res.Config), 0o640); err != nil { //nolint:gosec // Proxmox's cluster file system
			return notes, err
		}
		if err := r.ensureCloudInit(ctx, vmid, st.Config, storages); err != nil {
			return notes, fmt.Errorf("guest %d: %w", vmid, err)
		}
		for _, rm := range res.Removed {
			notes = append(notes, fmt.Sprintf("guest %d: removed %s", vmid, rm))
		}
	}
	return notes, nil
}

// ensureCloudInit gives a registered VM the cloud-init drive its original
// configuration has. If a replica of that drive exists (for example,
// replicated by an earlier hand-written setup), it's reused: Proxmox
// regenerates a cloud-init drive's contents when the VM starts. Otherwise a
// fresh drive is created.
func (r *Runner) ensureCloudInit(ctx context.Context, vmid uint32, original string, storages map[string]*clientv1.RecoveryStorage) error {
	key, vol := cloudInitDrive(original)
	if key == "" {
		return nil
	}
	b, err := os.ReadFile(r.configPath("qemu", vmid))
	if err != nil {
		return err
	}
	if value(string(b), key) != "" {
		return nil // already there
	}
	source, volname, _ := strings.Cut(vol, ":")
	s := storages[source]
	if s == nil {
		for _, other := range storages {
			s = other // any of the plan's storages can hold a new drive
			break
		}
	}
	if s == nil {
		return errors.New("no storage for the cloud-init drive")
	}
	id := strconv.FormatUint(uint64(vmid), 10)
	name := volname[strings.LastIndex(volname, "/")+1:]
	ds := s.ReceiveDataset + "/" + s.SourceDataset + "/" + name
	if storages[source] != nil {
		if _, err := r.Run(ctx, "zfs", "list", "-H", "-o", "name", ds); err == nil {
			if _, err := r.Run(ctx, "zfs", "set", "readonly=off", ds); err != nil {
				return err
			}
			if _, err := r.Run(ctx, "qm", "set", id, "--"+key, s.StorageId+":"+name+",media=cdrom"); err != nil {
				return fmt.Errorf("attach the cloud-init drive: %w", err)
			}
			return nil
		}
	}
	if _, err := r.Run(ctx, "qm", "set", id, "--"+key, s.StorageId+":cloudinit"); err != nil {
		return fmt.Errorf("create the cloud-init drive: %w", err)
	}
	return nil
}

// cloudInitDrive returns the key and volume of a VM configuration's
// cloud-init drive, if it has one.
func cloudInitDrive(conf string) (string, string) {
	for line := range strings.SplitSeq(conf, "\n") {
		key, v, ok := strings.Cut(line, ":")
		vol, opts, _ := strings.Cut(strings.TrimSpace(v), ",")
		if ok && strings.Contains(vol, "cloudinit") && strings.Contains(opts, "media=cdrom") {
			return strings.TrimSpace(key), vol
		}
	}
	return "", ""
}

// prepareReplica makes a replica usable as a guest's disk.
func (r *Runner) prepareReplica(ctx context.Context, ds, typ, size string) error {
	if _, err := r.Run(ctx, "zfs", "list", "-H", "-o", "name", ds); err != nil {
		return fmt.Errorf("replica %s doesn't exist", ds)
	}
	// A partially received stream would otherwise keep the dataset busy.
	if _, err := r.Run(ctx, "zfs", "receive", "-A", ds); err != nil && !strings.Contains(err.Error(), "resumable") {
		return err
	}
	if _, err := r.Run(ctx, "zfs", "set", "readonly=off", ds); err != nil {
		return err
	}
	// zrepl doesn't send properties, so a container volume's size (its
	// refquota) wasn't replicated.
	if typ == "lxc" && strings.Contains(ds, "/subvol-") && sizePattern.MatchString(size) {
		if _, err := r.Run(ctx, "zfs", "set", "refquota="+size, ds); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) ensureStorage(ctx context.Context, s *clientv1.RecoveryStorage, node string) error {
	ids, err := r.storageIDs()
	if err != nil {
		return err
	}
	if slices.Contains(ids, s.StorageId) {
		return nil
	}
	_, err = r.Run(ctx, "pvesm", "add", "zfspool", s.StorageId, "--pool", s.ReceiveDataset+"/"+s.SourceDataset,
		"--content", "images,rootdir", "--sparse", "1", "--nodes", node)
	return err
}

// storageIDs lists the storage IDs in Proxmox's storage configuration.
func (r *Runner) storageIDs() ([]string, error) {
	b, err := os.ReadFile(filepath.Join(r.PVE, "storage.cfg"))
	if err != nil {
		return nil, err
	}
	var ids []string
	for line := range strings.SplitSeq(string(b), "\n") {
		if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '#' {
			continue
		}
		if _, id, ok := strings.Cut(line, ":"); ok {
			ids = append(ids, strings.TrimSpace(id))
		}
	}
	return ids, nil
}

func (r *Runner) usedVMIDs() (map[uint32]bool, error) {
	b, err := os.ReadFile(filepath.Join(r.PVE, ".vmlist"))
	if err != nil {
		return nil, err
	}
	var list struct {
		IDs map[string]json.RawMessage `json:"ids"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("read the cluster's guest list: %w", err)
	}
	used := map[uint32]bool{}
	for id := range list.IDs {
		if n, err := strconv.ParseUint(id, 10, 32); err == nil {
			used[uint32(n)] = true
		}
	}
	return used, nil
}

// oursOnDR checks that a guest on the DR host was registered by this plan's
// failover.
func (r *Runner) oursOnDR(planID, typ string, vmid uint32) error {
	b, err := os.ReadFile(r.configPath(typ, vmid))
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("guest %d isn't on this host", vmid)
	}
	if err != nil {
		return err
	}
	conf := string(b)
	if !strings.Contains(description(conf), marker(planID)) || !slices.Contains(tags(conf), Tag) {
		return fmt.Errorf("guest %d wasn't registered by this plan's failover", vmid)
	}
	return nil
}

// StartGuest starts one of the plan's failed-over guests.
func (r *Runner) StartGuest(ctx context.Context, planID string, vmid uint32) error {
	typ, _, err := r.guest(vmid)
	if err != nil {
		return err
	}
	if err := r.oursOnDR(planID, typ, vmid); err != nil {
		return err
	}
	if running, _ := r.running(ctx, typ, vmid); running {
		return nil
	}
	_, err = r.Run(ctx, tool(typ), "start", strconv.FormatUint(uint64(vmid), 10))
	return err
}

// CheckGuest reports whether a failed-over guest runs and, for VMs with the
// guest agent enabled, whether the agent answers.
func (r *Runner) CheckGuest(ctx context.Context, planID string, vmid uint32) (*clientv1.TestGuestCheck, error) {
	typ, conf, err := r.guest(vmid)
	if err != nil {
		return nil, err
	}
	if err := r.oursOnDR(planID, typ, vmid); err != nil {
		return nil, err
	}
	c := &clientv1.TestGuestCheck{}
	if c.Running, err = r.running(ctx, typ, vmid); err != nil {
		return nil, err
	}
	if typ == "qemu" {
		agent := value(conf, "agent")
		first, _, _ := strings.Cut(agent, ",")
		c.AgentEnabled = first == "1" || strings.Contains(","+agent+",", ",enabled=1,")
		if c.AgentEnabled && c.Running {
			pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			_, err := r.Run(pctx, "qm", "guest", "cmd", strconv.FormatUint(uint64(vmid), 10), "ping")
			cancel()
			c.AgentOk = err == nil
		}
	}
	return c, nil
}

// description returns a configuration's leading comment lines, decoded:
// Proxmox percent-encodes some characters in VM descriptions.
func description(conf string) string {
	var lines []string
	for line := range strings.SplitSeq(conf, "\n") {
		d, ok := strings.CutPrefix(line, "#")
		if !ok {
			break
		}
		if u, err := url.PathUnescape(d); err == nil {
			d = u
		}
		lines = append(lines, d)
	}
	return strings.Join(lines, "\n")
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
