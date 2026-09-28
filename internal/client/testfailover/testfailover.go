// Package testfailover runs test failovers on the DR host: it clones
// replicas into a test storage, registers test guests from the stored
// configurations, starts and checks them, and cleans up. See
// docs/design/test-failover.md.
package testfailover

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jlbyh2o/ezdr/internal/client/guests"
	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
	"github.com/jlbyh2o/ezdr/internal/guestconfig"
)

// Tag marks test guests.
const Tag = "ezdr-test"

// Runner performs test failover actions. Tests replace its fields.
type Runner struct {
	// PVE is Proxmox's cluster file system root.
	PVE    string
	Guests guests.Paths
	// MemInfo is /proc/meminfo.
	MemInfo string
	Run     func(ctx context.Context, name string, args ...string) ([]byte, error)
	Node    func() (string, error)
}

// NewRunner returns a Runner for the real host.
func NewRunner() *Runner {
	return &Runner{PVE: "/etc/pve", Guests: guests.DefaultPaths, MemInfo: "/proc/meminfo", Run: runCommand, Node: nodeName}
}

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // fixed commands from this package
	cmd.Dir = "/"
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return out, fmt.Errorf("%s: %w: %s", name, err, msg)
		}
		return out, fmt.Errorf("%s: %w", name, err)
	}
	return out, nil
}

func nodeName() (string, error) {
	h, err := os.Hostname()
	short, _, _ := strings.Cut(h, ".")
	return short, err
}

var (
	datasetPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]*(/[A-Za-z0-9_.:-]+)+$`)
	volumeName     = regexp.MustCompile(`^(vm|subvol)-\d+-[A-Za-z0-9_.-]+$`)
	snapshotName   = regexp.MustCompile(`^[A-Za-z0-9_.:-]+$`)
	testIDPattern  = regexp.MustCompile(`^[a-z0-9]{1,40}$`)
	storageInvalid = regexp.MustCompile(`[^a-z0-9-]+`)
)

// StorageID is the test storage for a pool, such as "ezdr-test-tank".
func StorageID(pool string) string {
	return "ezdr-test-" + strings.Trim(storageInvalid.ReplaceAllString(strings.ToLower(pool), "-"), "-")
}

// testDataset is the dataset holding a pool's test clones.
func testDataset(pool string) string { return pool + "/" + Tag }

func poolOf(dataset string) string {
	p, _, _ := strings.Cut(dataset, "/")
	return p
}

// marker identifies a test's guests in their description.
func marker(testID string) string { return "EZDR test ID: " + testID }

// Options lists the snapshots every replica has, newest first, and the
// host's available memory.
func (r *Runner) Options(ctx context.Context, replicas []string, prefix string) (*clientv1.TestOptionsResult, error) {
	res := &clientv1.TestOptionsResult{}
	var common map[string]int64
	for _, rep := range replicas {
		if !datasetPattern.MatchString(rep) {
			return nil, fmt.Errorf("invalid dataset %q", rep)
		}
		out, err := r.Run(ctx, "zfs", "list", "-Hp", "-t", "snapshot", "-o", "name,creation", "-d", "1", rep)
		if err != nil {
			return nil, err
		}
		have := map[string]int64{}
		for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
			name, created, ok := strings.Cut(line, "\t")
			_, snap, isSnap := strings.Cut(name, "@")
			if !ok || !isSnap || !strings.HasPrefix(snap, prefix) {
				continue
			}
			at, _ := strconv.ParseInt(created, 10, 64)
			have[snap] = at
		}
		if common == nil {
			common = have
			continue
		}
		for s := range common {
			if _, ok := have[s]; !ok {
				delete(common, s)
			}
		}
	}
	for s, at := range common {
		res.Snapshots = append(res.Snapshots, &clientv1.TestSnapshot{Name: s, CreatedAt: timestamppb.New(time.Unix(at, 0))})
	}
	slices.SortFunc(res.Snapshots, func(a, b *clientv1.TestSnapshot) int {
		return b.CreatedAt.AsTime().Compare(a.CreatedAt.AsTime())
	})
	res.MemoryAvailableBytes = r.memAvailable()
	return res, nil
}

func (r *Runner) memAvailable() uint64 {
	f, err := os.Open(r.MemInfo)
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "MemAvailable:"); ok {
			kb, _ := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
			return kb << 10
		}
	}
	return 0
}

// Prepare creates the test storage, clones the guests' disks, and registers
// the test guests. It returns notes, such as settings dropped from a guest.
func (r *Runner) Prepare(ctx context.Context, p *clientv1.TestPrepare) ([]string, error) {
	if !testIDPattern.MatchString(p.TestId) || !snapshotName.MatchString(p.Snapshot) || p.Bridge == "" {
		return nil, errors.New("invalid test")
	}
	stored, err := guests.Load(r.Guests, p.PlanId)
	if err != nil {
		return nil, err
	}
	for _, g := range p.Guests {
		if err := checkGuest(g); err != nil {
			return nil, err
		}
		if _, ok := stored[g.Vmid]; !ok {
			return nil, fmt.Errorf("guest %d: the DR host has no copy of its configuration", g.Vmid)
		}
	}
	node, err := r.Node()
	if err != nil {
		return nil, err
	}
	pools := map[string]bool{}
	for _, g := range p.Guests {
		for _, d := range g.Disks {
			pools[poolOf(d.Replica)] = true
		}
	}
	for pool := range pools {
		if err := r.ensureStorage(ctx, pool, node); err != nil {
			return nil, err
		}
	}
	storages, err := r.storages()
	if err != nil {
		return nil, err
	}
	used, err := r.usedVMIDs()
	if err != nil {
		return nil, err
	}

	var notes []string
	for _, g := range p.Guests {
		n, err := r.prepareGuest(ctx, p, g, stored[g.Vmid], storages, used)
		if err != nil {
			return notes, fmt.Errorf("guest %d: %w", g.Vmid, err)
		}
		notes = append(notes, n...)
	}
	return notes, nil
}

func checkGuest(g *clientv1.TestGuest) error {
	if g.Vmid == 0 || g.TestVmid == 0 || (g.Type != "qemu" && g.Type != "lxc") {
		return fmt.Errorf("invalid guest %d", g.Vmid)
	}
	for _, d := range g.Disks {
		if !datasetPattern.MatchString(d.Replica) || !volumeName.MatchString(d.Clone) ||
			!strings.Contains(d.Clone, "-"+strconv.FormatUint(uint64(g.TestVmid), 10)+"-") {
			return fmt.Errorf("guest %d: invalid disk %v", g.Vmid, d)
		}
	}
	return nil
}

func (r *Runner) prepareGuest(ctx context.Context, p *clientv1.TestPrepare, g *clientv1.TestGuest, st guests.Stored,
	storages []string, used map[uint32]bool) ([]string, error) {
	conf := r.configPath(g.Type, g.TestVmid)
	if used[g.TestVmid] {
		// Registered by an earlier attempt of this test?
		if err := r.ours(p.TestId, g.Type, g.TestVmid); err != nil {
			return nil, fmt.Errorf("ID %d is already in use: %w", g.TestVmid, err)
		}
		return nil, nil
	}
	volumes := map[string]string{}
	for _, d := range g.Disks {
		pool := poolOf(d.Replica)
		target := testDataset(pool) + "/" + d.Clone
		origin := d.Replica + "@" + p.Snapshot
		if out, err := r.Run(ctx, "zfs", "get", "-H", "-o", "value", "origin", target); err == nil {
			if got := strings.TrimSpace(string(out)); got != origin {
				return nil, fmt.Errorf("%s already exists and isn't a clone of %s", target, origin)
			}
		} else if _, err := r.Run(ctx, "zfs", "clone", origin, target); err != nil {
			return nil, err
		}
		volumes[d.Volume] = StorageID(pool) + ":" + d.Clone
	}
	name := ""
	if g.Type == "qemu" {
		name = "test-" + configValue(st.Config, "name")
		if name == "test-" {
			name = "test-" + strconv.FormatUint(uint64(g.Vmid), 10)
		}
	}
	res, err := guestconfig.Rewrite(st.Config, guestconfig.Mapping{
		Type: g.Type, Volumes: volumes, Bridge: p.Bridge, Storages: storages, Name: name, Tag: Tag,
		Note:    fmt.Sprintf("Test failover of guest %d from plan %s. %s", g.Vmid, oneLine(p.PlanName), marker(p.TestId)),
		VMGenID: newUUID(),
	})
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(conf, []byte(res.Config), 0o640); err != nil { //nolint:gosec // Proxmox's cluster file system
		return nil, err
	}
	if res.CloudInit != "" && len(g.Disks) > 0 {
		store := StorageID(poolOf(g.Disks[0].Replica))
		if _, err := r.Run(ctx, "qm", "set", strconv.FormatUint(uint64(g.TestVmid), 10), "--"+res.CloudInit, store+":cloudinit"); err != nil {
			return nil, fmt.Errorf("create the cloud-init drive: %w", err)
		}
	}
	var notes []string
	for _, rm := range res.Removed {
		notes = append(notes, fmt.Sprintf("guest %d: removed %s", g.Vmid, rm))
	}
	return notes, nil
}

// ensureStorage creates a pool's test dataset and Proxmox storage.
func (r *Runner) ensureStorage(ctx context.Context, pool, node string) error {
	ds := testDataset(pool)
	if _, err := r.Run(ctx, "zfs", "list", "-H", "-o", "name", ds); err != nil {
		if _, err := r.Run(ctx, "zfs", "create", ds); err != nil {
			return err
		}
	}
	storages, err := r.storages()
	if err != nil {
		return err
	}
	if slices.Contains(storages, StorageID(pool)) {
		return nil
	}
	_, err = r.Run(ctx, "pvesm", "add", "zfspool", StorageID(pool), "--pool", ds, "--content", "images,rootdir",
		"--sparse", "1", "--nodes", node)
	return err
}

// storages lists the storage IDs in Proxmox's storage configuration.
func (r *Runner) storages() ([]string, error) {
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

// usedVMIDs lists every VMID in the cluster.
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

func (r *Runner) configPath(typ string, vmid uint32) string {
	dir := "qemu-server"
	if typ == "lxc" {
		dir = "lxc"
	}
	return filepath.Join(r.PVE, dir, strconv.FormatUint(uint64(vmid), 10)+".conf")
}

// errGone means the test guest doesn't exist (any more).
var errGone = errors.New("the test guest doesn't exist")

// ours checks that a guest is one of the test's guests: tagged, marked with
// the test ID, and with every disk on a test storage. Anything else is
// refused, so test actions can never touch other guests.
func (r *Runner) ours(testID, typ string, vmid uint32) error {
	b, err := os.ReadFile(r.configPath(typ, vmid))
	if errors.Is(err, os.ErrNotExist) {
		return errGone
	}
	if err != nil {
		return err
	}
	conf := string(b)
	if !strings.Contains(conf, marker(testID)) {
		return fmt.Errorf("guest %d isn't part of test %s", vmid, testID)
	}
	if !slices.Contains(strings.Split(configValue(conf, "tags"), ";"), Tag) {
		return fmt.Errorf("guest %d isn't tagged %s", vmid, Tag)
	}
	for line := range strings.SplitSeq(conf, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok || !isDiskKey(typ, strings.TrimSpace(key)) {
			continue
		}
		vol, opts, _ := strings.Cut(strings.TrimSpace(value), ",")
		if vol == "none" || vol == "cdrom" || (strings.Contains(opts, "media=cdrom") && !strings.Contains(vol, "cloudinit")) {
			continue
		}
		if !strings.HasPrefix(vol, "ezdr-test-") {
			return fmt.Errorf("guest %d has a disk outside the test storage (%s)", vmid, vol)
		}
	}
	return nil
}

var (
	vmDiskKey = regexp.MustCompile(`^((ide|sata|scsi|virtio)\d+|efidisk0|tpmstate0|unused\d+)$`)
	ctDiskKey = regexp.MustCompile(`^(rootfs|mp\d+|unused\d+)$`)
)

func isDiskKey(typ, key string) bool {
	if typ == "qemu" {
		return vmDiskKey.MatchString(key)
	}
	return ctDiskKey.MatchString(key)
}

// StartGuest starts one of the test's guests.
func (r *Runner) StartGuest(ctx context.Context, testID, typ string, vmid uint32) error {
	if err := r.ours(testID, typ, vmid); err != nil {
		return err
	}
	if running, _ := r.running(ctx, typ, vmid); running {
		return nil
	}
	_, err := r.Run(ctx, tool(typ), "start", strconv.FormatUint(uint64(vmid), 10))
	return err
}

// CheckGuest reports whether a test guest runs and, for VMs with the guest
// agent enabled, whether the agent answers.
func (r *Runner) CheckGuest(ctx context.Context, testID, typ string, vmid uint32) (*clientv1.TestGuestCheck, error) {
	if err := r.ours(testID, typ, vmid); err != nil {
		return nil, err
	}
	c := &clientv1.TestGuestCheck{}
	var err error
	if c.Running, err = r.running(ctx, typ, vmid); err != nil {
		return nil, err
	}
	if typ == "qemu" {
		b, _ := os.ReadFile(r.configPath(typ, vmid))
		agent := configValue(string(b), "agent")
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

func (r *Runner) running(ctx context.Context, typ string, vmid uint32) (bool, error) {
	out, err := r.Run(ctx, tool(typ), "status", strconv.FormatUint(uint64(vmid), 10))
	if err != nil {
		return false, err
	}
	return strings.Contains(string(out), "status: running"), nil
}

// Cleanup stops and destroys the test's guests and their clones. Guests and
// clones that are already gone are skipped.
func (r *Runner) Cleanup(ctx context.Context, c *clientv1.TestCleanup) ([]string, error) {
	if !testIDPattern.MatchString(c.TestId) {
		return nil, errors.New("invalid test")
	}
	var done []string
	for _, g := range c.Guests {
		if err := checkGuest(g); err != nil {
			return done, err
		}
		id := strconv.FormatUint(uint64(g.TestVmid), 10)
		switch err := r.ours(c.TestId, g.Type, g.TestVmid); {
		case errors.Is(err, errGone):
		case err != nil:
			return done, err
		default:
			if running, _ := r.running(ctx, g.Type, g.TestVmid); running {
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
		// Clones the guest's destruction didn't remove (for example, if
		// registration failed halfway).
		for _, d := range g.Disks {
			target := testDataset(poolOf(d.Replica)) + "/" + d.Clone
			out, err := r.Run(ctx, "zfs", "get", "-H", "-o", "value", "origin", target)
			if err != nil {
				continue // gone
			}
			if !strings.HasPrefix(strings.TrimSpace(string(out)), d.Replica+"@") {
				return done, fmt.Errorf("%s isn't a clone of %s; not destroying it", target, d.Replica)
			}
			if _, err := r.Run(ctx, "zfs", "destroy", target); err != nil {
				return done, err
			}
			done = append(done, "destroyed clone "+target)
		}
	}
	return done, nil
}

func tool(typ string) string {
	if typ == "lxc" {
		return "pct"
	}
	return "qm"
}

// configValue returns a top-level setting from a configuration.
func configValue(conf, key string) string {
	for line := range strings.SplitSeq(conf, "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
