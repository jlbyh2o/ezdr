// Package cleanup removes replicated data EZDR no longer needs: a deleted
// plan's replicas and snapshots, and what a takeover's old zrepl jobs left
// behind. See docs/design/cleanup.md.
package cleanup

import (
	"bufio"
	"bytes"
	"context"
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

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
)

// Runner scans and cleans up datasets. Tests replace its fields.
type Runner struct {
	// PVE is Proxmox's cluster file system root.
	PVE string
	Run func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// NewRunner returns a Runner for the real host.
func NewRunner() *Runner {
	return &Runner{PVE: "/etc/pve", Run: runCommand}
}

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // fixed commands from this package
	cmd.Dir = "/"
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
		}
		return out, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return out, nil
}

var cleanupPrefix = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{1,30}[_.:-]$`)

var datasetPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]*(/[A-Za-z0-9_.:-]+)*$`)

// within reports whether dataset a is b or inside it.
func within(a, b string) bool { return a == b || strings.HasPrefix(a, b+"/") }

// host is what a scan needs to know about the host.
type host struct {
	used   map[string]uint64 // dataset -> used bytes
	origin map[string]string // clone -> origin snapshot
	// storages maps zfspool storages to their datasets.
	storages map[string]string
	// paths maps storages with a directory (such as dir storages) to it.
	paths map[string]string
	// mounts maps mounted datasets to their mount points.
	mounts map[string]string
	// volumes maps datasets guests use to the guests' VMIDs.
	volumes map[string]uint32
}

func (r *Runner) host(ctx context.Context) (*host, error) {
	out, err := r.Run(ctx, "zfs", "list", "-Hp", "-t", "filesystem,volume", "-o", "name,used,origin,mountpoint")
	if err != nil {
		return nil, err
	}
	h := &host{used: map[string]uint64{}, origin: map[string]string{}, storages: map[string]string{}, volumes: map[string]uint32{},
		paths: map[string]string{}, mounts: map[string]string{}}
	for _, line := range lines(out) {
		f := strings.Split(line, "\t")
		if len(f) != 4 {
			continue
		}
		h.used[f[0]], _ = strconv.ParseUint(f[1], 10, 64)
		if f[2] != "-" {
			h.origin[f[0]] = f[2]
		}
		if strings.HasPrefix(f[3], "/") {
			h.mounts[f[0]] = filepath.Clean(f[3])
		}
	}
	if err := h.readStorages(filepath.Join(r.PVE, "storage.cfg")); err != nil {
		return nil, err
	}
	confs, err := filepath.Glob(filepath.Join(r.PVE, "nodes", "*", "*", "*.conf"))
	if err != nil {
		return nil, err
	}
	for _, c := range confs {
		dir := filepath.Base(filepath.Dir(c))
		if dir != "qemu-server" && dir != "lxc" {
			continue
		}
		id, err := strconv.ParseUint(strings.TrimSuffix(filepath.Base(c), ".conf"), 10, 32)
		if err != nil {
			continue
		}
		b, err := os.ReadFile(c) //nolint:gosec // Proxmox guest configurations
		if err != nil {
			return nil, err
		}
		for _, ds := range h.guestDatasets(string(b)) {
			h.volumes[ds] = uint32(id)
		}
	}
	return h, nil
}

// readStorages reads zfspool storages' datasets and other storages'
// directories from storage.cfg.
func (h *host) readStorages(path string) error {
	b, err := os.ReadFile(path) //nolint:gosec // Proxmox storage configuration
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	id, zfspool := "", false
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := sc.Text()
		if typ, name, ok := strings.Cut(line, ":"); ok && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			id, zfspool = strings.TrimSpace(name), strings.TrimSpace(typ) == "zfspool"
			continue
		}
		k, v, ok := strings.Cut(strings.TrimSpace(line), " ")
		switch {
		case !ok || id == "":
		case k == "pool" && zfspool:
			h.storages[id] = strings.TrimSpace(v)
		case k == "path":
			h.paths[id] = filepath.Clean(strings.TrimSpace(v))
		}
	}
	return sc.Err()
}

// guestDatasets returns the datasets of the zfspool volumes a guest
// configuration refers to, such as "local-zfs:vm-100-disk-0".
func (h *host) guestDatasets(conf string) []string {
	var out []string
	for _, line := range strings.Split(conf, "\n") {
		_, v, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		for _, item := range strings.Split(v, ",") {
			if _, after, ok := strings.Cut(item, "="); ok && (strings.HasPrefix(item, "file=") || strings.HasPrefix(item, "volume=")) {
				item = after
			}
			storage, vol, ok := strings.Cut(item, ":")
			if pool := h.storages[storage]; ok && pool != "" && vol != "" {
				out = append(out, pool+"/"+vol)
			}
		}
	}
	return out
}

// plan is what cleaning up one target removes.
type plan struct {
	scan      *clientv1.DatasetScan
	snapshots []string // names without "@", for snapshot targets
	bookmarks []string // names without "#"
}

// ownHold reports whether a hold belongs to one of the jobs, whose holds
// the cleanup releases (zrepl names them "..._J_<job>").
func ownHold(tag string, jobs []string) bool {
	for _, j := range jobs {
		if strings.HasSuffix(tag, "_J_"+j) {
			return true
		}
	}
	return false
}

// holds returns each snapshot's holds.
func (r *Runner) holds(ctx context.Context, snapshots []string) (map[string][]string, error) {
	held := map[string][]string{}
	for chunk := range slices.Chunk(snapshots, 100) {
		out, err := r.Run(ctx, "zfs", append([]string{"holds", "-H"}, chunk...)...)
		if err != nil {
			return nil, err
		}
		for _, line := range lines(out) {
			f := strings.Split(line, "\t")
			if len(f) >= 2 {
				held[f[0]] = append(held[f[0]], f[1])
			}
		}
	}
	return held, nil
}

func (r *Runner) planTarget(ctx context.Context, h *host, t *clientv1.DataTarget, jobs []string) (*plan, error) {
	s := &clientv1.DatasetScan{Dataset: t.Dataset}
	p := &plan{scan: s}
	if !datasetPattern.MatchString(t.Dataset) {
		s.Problems = append(s.Problems, "not a valid dataset name")
		return p, nil
	}
	used, ok := h.used[t.Dataset]
	if !ok {
		return p, nil
	}
	s.Exists = true
	depth := []string{"-d", "1"}
	if t.Destroy {
		depth = []string{"-r"}
		if !strings.Contains(t.Dataset, "/") {
			s.Problems = append(s.Problems, "a pool can't be destroyed")
			return p, nil
		}
		for clone, origin := range h.origin {
			ds, _, _ := strings.Cut(origin, "@")
			if within(ds, t.Dataset) && !within(clone, t.Dataset) {
				s.Problems = append(s.Problems, fmt.Sprintf("%s is a clone of %s", clone, origin))
			}
		}
		for id, pool := range h.storages {
			if within(pool, t.Dataset) {
				s.Problems = append(s.Problems, "used by storage "+id)
			}
		}
		// A directory storage on a dataset being destroyed (such as
		// backups on a dir storage).
		for ds, mnt := range h.mounts {
			if !within(ds, t.Dataset) {
				continue
			}
			for id, path := range h.paths {
				if mnt == "/" || path == mnt || strings.HasPrefix(path, mnt+"/") {
					s.Problems = append(s.Problems, fmt.Sprintf("storage %s is in %s", id, ds))
				}
			}
		}
		for ds, vmid := range h.volumes {
			if within(ds, t.Dataset) {
				s.Problems = append(s.Problems, fmt.Sprintf("used by guest %d (%s)", vmid, ds))
			}
		}
	} else if !cleanupPrefix.MatchString(t.Prefix) {
		// Snapshots are deleted by prefix: it must look like a replication
		// tool's, such as "ezdr_" or "zrepl_".
		s.Problems = append(s.Problems, fmt.Sprintf("snapshot prefix %q is too broad to delete snapshots by", t.Prefix))
		return p, nil
	}
	out, err := r.Run(ctx, "zfs", append(append([]string{"list", "-Hp", "-t", "snapshot,bookmark", "-o", "name,clones"}, depth...), t.Dataset)...)
	if err != nil {
		return nil, err
	}
	var snaps []string
	clones := map[string]bool{}
	for _, line := range lines(out) {
		name, cl, _ := strings.Cut(line, "\t")
		if ds, bm, ok := strings.Cut(name, "#"); ok {
			if t.Destroy || (ds == t.Dataset && strings.HasPrefix(bm, t.Prefix)) {
				p.bookmarks = append(p.bookmarks, bm)
			}
			continue
		}
		_, snap, _ := strings.Cut(name, "@")
		if t.Destroy || strings.HasPrefix(snap, t.Prefix) {
			snaps = append(snaps, name)
			clones[name] = cl != "" && cl != "-"
		}
	}
	held, err := r.holds(ctx, snaps)
	if err != nil {
		return nil, err
	}
	for _, name := range snaps {
		var foreign []string
		for _, tag := range held[name] {
			if !ownHold(tag, jobs) {
				foreign = append(foreign, tag)
			}
		}
		switch {
		case len(foreign) > 0 && t.Destroy:
			s.Problems = append(s.Problems, fmt.Sprintf("%s is held by %s", name, strings.Join(foreign, ", ")))
		case len(foreign) > 0:
			s.Skipped = append(s.Skipped, fmt.Sprintf("%s (held by %s)", name, strings.Join(foreign, ", ")))
		case clones[name] && !t.Destroy:
			s.Skipped = append(s.Skipped, name+" (has clones)")
		default:
			_, snap, _ := strings.Cut(name, "@")
			p.snapshots = append(p.snapshots, snap)
		}
	}
	s.Snapshots = uint32(len(p.snapshots)) //nolint:gosec // snapshot counts are small
	s.Bookmarks = uint32(len(p.bookmarks)) //nolint:gosec // bookmark counts are small
	if t.Destroy {
		s.ReclaimBytes = used
		return p, nil
	}
	if len(p.snapshots) > 0 {
		out, err := r.Run(ctx, "zfs", "destroy", "-nvp", t.Dataset+"@"+strings.Join(p.snapshots, ","))
		if err != nil {
			return nil, err
		}
		for _, line := range lines(out) {
			if v, ok := strings.CutPrefix(line, "reclaim\t"); ok {
				s.ReclaimBytes, _ = strconv.ParseUint(v, 10, 64)
			}
		}
	}
	return p, nil
}

func (r *Runner) plans(ctx context.Context, targets []*clientv1.DataTarget, jobs []string) ([]*plan, error) {
	h, err := r.host(ctx)
	if err != nil {
		return nil, err
	}
	var out []*plan
	for _, t := range targets {
		p, err := r.planTarget(ctx, h, t, jobs)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", t.Dataset, err)
		}
		out = append(out, p)
	}
	return out, nil
}

// Scan reports what Clean would remove, changing nothing.
func (r *Runner) Scan(ctx context.Context, targets []*clientv1.DataTarget, jobs []string) (*clientv1.DataScanResult, error) {
	plans, err := r.plans(ctx, targets, jobs)
	if err != nil {
		return nil, err
	}
	res := &clientv1.DataScanResult{}
	for _, p := range plans {
		res.Datasets = append(res.Datasets, p.scan)
	}
	return res, nil
}

// Clean releases the jobs' holds and cursors and removes the targets. It
// removes nothing if a target has problems. It returns what it did.
func (r *Runner) Clean(ctx context.Context, targets []*clientv1.DataTarget, jobs []string) ([]string, error) {
	plans, err := r.plans(ctx, targets, jobs)
	if err != nil {
		return nil, err
	}
	var problems []string
	for _, p := range plans {
		for _, pr := range p.scan.Problems {
			problems = append(problems, p.scan.Dataset+": "+pr)
		}
	}
	if len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "; "))
	}
	var events []string
	for _, j := range jobs {
		if _, err := r.Run(ctx, "zrepl", "zfs-abstraction", "release-all", "--job", j); err != nil {
			return events, err
		}
		events = append(events, "released the holds and cursors of zrepl job "+j)
	}
	for i, t := range targets {
		p := plans[i]
		if !p.scan.Exists {
			continue
		}
		if t.Destroy {
			if _, err := r.Run(ctx, "zfs", "destroy", "-r", t.Dataset); err != nil {
				return events, err
			}
			events = append(events, fmt.Sprintf("destroyed %s (%s)", t.Dataset, size(p.scan.ReclaimBytes)))
			ev, err := r.removeEmptyParents(ctx, t.Dataset, t.EmptyParentsBelow)
			events = append(events, ev...)
			if err != nil {
				return events, err
			}
			continue
		}
		for chunk := range slices.Chunk(p.snapshots, 50) {
			if _, err := r.Run(ctx, "zfs", "destroy", t.Dataset+"@"+strings.Join(chunk, ",")); err != nil {
				return events, err
			}
		}
		removed := 0
		for _, bm := range p.bookmarks {
			// Releasing the jobs may have removed zrepl's cursors already.
			if _, err := r.Run(ctx, "zfs", "destroy", t.Dataset+"#"+bm); err != nil {
				if exists, _ := r.exists(ctx, t.Dataset+"#"+bm); exists {
					return events, err
				}
				continue
			}
			removed++
		}
		events = append(events, fmt.Sprintf("deleted %d snapshot(s) and %d bookmark(s) on %s (%s)", len(p.snapshots), removed, t.Dataset, size(p.scan.ReclaimBytes)))
	}
	return events, nil
}

// removeEmptyParents destroys the parents of a destroyed dataset that are
// now empty (no children, no snapshots, no storage), up to but not
// including below.
func (r *Runner) removeEmptyParents(ctx context.Context, dataset, below string) ([]string, error) {
	if below == "" || !within(dataset, below) {
		return nil, nil
	}
	h, err := r.host(ctx)
	if err != nil {
		return nil, err
	}
	var events []string
	for p := filepath.Dir(dataset); p != below && within(p, below); p = filepath.Dir(p) {
		out, err := r.Run(ctx, "zfs", "list", "-H", "-t", "all", "-o", "name", "-d", "1", p)
		if err != nil {
			return events, err
		}
		if len(lines(out)) > 1 {
			return events, nil // children, snapshots, or bookmarks
		}
		for _, pool := range h.storages {
			if within(pool, p) {
				return events, nil
			}
		}
		if _, err := r.Run(ctx, "zfs", "destroy", p); err != nil {
			return events, err
		}
		events = append(events, "destroyed empty "+p)
	}
	return events, nil
}

func (r *Runner) exists(ctx context.Context, name string) (bool, error) {
	_, err := r.Run(ctx, "zfs", "list", "-H", "-t", "all", "-o", "name", name)
	return err == nil, err
}

func lines(b []byte) []string {
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// size formats a byte count, such as "1.5 GiB".
func size(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
