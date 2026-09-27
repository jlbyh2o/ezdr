package zrepl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
)

// Taking over an existing zrepl setup (docs/design/replication.md, section
// 5). The portal runs these steps in order on both hosts; each is safe to
// repeat.

var (
	backupID = regexp.MustCompile(`^[a-z0-9]{1,40}$`)
	// jobName accepts hand-written job names, which the takeover passes to
	// zrepl's command line.
	jobName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$`)
)

// backupPath is where RemoveJobs keeps the original main configuration.
func (p Paths) backupPath(id string) (string, error) {
	if !backupID.MatchString(id) {
		return "", fmt.Errorf("invalid backup name %q", id)
	}
	return p.MainConfig + ".ezdr-takeover-" + id, nil
}

// Preflight lists the datasets' snapshots and bookmarks and previews
// releasing releaseJob's holds and bookmarks. It changes nothing.
func (a *Applier) Preflight(ctx context.Context, datasets []string, releaseJob string) (*clientv1.ZreplPreflightResult, error) {
	res := &clientv1.ZreplPreflightResult{ZreplVersion: a.Version(ctx)}
	_, err := a.Run(ctx, "systemctl", "is-active", "--quiet", "zrepl")
	res.ZreplRunning = err == nil
	for _, ds := range datasets {
		if !validDataset(datasetPattern, ds) {
			return nil, fmt.Errorf("invalid dataset name %q", ds)
		}
		d := &clientv1.DatasetSnapshots{Dataset: ds}
		out, err := a.Run(ctx, "zfs", "list", "-Hp", "-t", "filesystem,volume,snapshot,bookmark", "-d", "1",
			"-o", "name,guid,createtxg,referenced", ds)
		if err != nil {
			if strings.Contains(err.Error(), "does not exist") {
				res.Datasets = append(res.Datasets, d)
				continue
			}
			return nil, fmt.Errorf("list %s: %w", ds, err)
		}
		d.Exists = true
		for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
			f := strings.Split(line, "\t")
			if len(f) != 4 {
				continue
			}
			guid, _ := strconv.ParseUint(f[1], 10, 64)
			txg, _ := strconv.ParseUint(f[2], 10, 64)
			if f[0] == ds {
				d.ReferencedBytes, _ = strconv.ParseUint(f[3], 10, 64)
				continue
			}
			if name, ok := strings.CutPrefix(f[0], ds); ok && (strings.HasPrefix(name, "@") || strings.HasPrefix(name, "#")) {
				d.Snapshots = append(d.Snapshots, &clientv1.SnapshotInfo{Name: name, Guid: guid, Createtxg: txg})
			}
		}
		slices.SortStableFunc(d.Snapshots, func(x, y *clientv1.SnapshotInfo) int {
			return int(x.Createtxg) - int(y.Createtxg) //nolint:gosec // txg differences fit
		})
		res.Datasets = append(res.Datasets, d)
	}
	if releaseJob != "" && res.ZreplVersion != "" {
		if !jobName.MatchString(releaseJob) {
			return nil, fmt.Errorf("invalid job name %q", releaseJob)
		}
		out, err := a.Run(ctx, "zrepl", "zfs-abstraction", "release-all", "--job", releaseJob, "--dry-run")
		if err != nil {
			return nil, fmt.Errorf("preview releasing %s: %w", releaseJob, err)
		}
		res.ReleasePreview = lines(out)
	}
	return res, nil
}

// RemoveJobs removes hand-written jobs from the main configuration, makes it
// include EZDR's jobs directory, and restarts zrepl. The original main
// configuration is kept under backup (a retry keeps the first backup). Jobs
// defined in other files are refused, since EZDR only edits the main file.
// If zrepl rejects the result, the original is restored.
func (a *Applier) RemoveJobs(ctx context.Context, jobs []string, backup string) error {
	bp, err := a.Paths.backupPath(backup)
	if err != nil {
		return err
	}
	all, err := ReadJobs(a.Paths)
	if err != nil {
		return fmt.Errorf("read zrepl configuration: %w", err)
	}
	for _, j := range all {
		if slices.Contains(jobs, j.Name) && j.File != a.Paths.MainConfig {
			return fmt.Errorf("job %s is defined in %s; only jobs in %s can be taken over", j.Name, j.File, a.Paths.MainConfig)
		}
	}
	orig, err := os.ReadFile(a.Paths.MainConfig)
	if err != nil {
		return err
	}
	if _, err := os.Stat(bp); errors.Is(err, os.ErrNotExist) {
		if err := writeFile(bp, orig, 0o644); err != nil {
			return err
		}
	}
	// The jobs file must exist before the main configuration includes it.
	if err := os.MkdirAll(a.Paths.JobsDir, 0o750); err != nil {
		return err
	}
	if _, err := os.Stat(a.Paths.JobsFile()); errors.Is(err, os.ErrNotExist) {
		if err := writeFile(a.Paths.JobsFile(), []byte("jobs: []\n"), 0o644); err != nil {
			return err
		}
	}
	edited, changed, err := editMainConfig(a.Paths, orig, jobs)
	if err != nil {
		return err
	}
	if changed {
		if err := writeFile(a.Paths.MainConfig, edited, 0o644); err != nil {
			return err
		}
		if out, err := a.Run(ctx, "zrepl", "configcheck"); err != nil {
			_ = writeFile(a.Paths.MainConfig, orig, 0o644)
			return fmt.Errorf("zrepl rejected the edited configuration (original kept): %w %s", err, strings.TrimSpace(string(out)))
		}
	}
	if _, err := a.Run(ctx, "systemctl", "restart", "zrepl"); err != nil {
		return fmt.Errorf("restart zrepl: %w", err)
	}
	return a.ensureRunning(ctx)
}

// RestoreConfig puts back the main configuration saved by RemoveJobs and
// restarts zrepl. If zrepl rejects it, it's still restored (it's the
// original) but zrepl isn't restarted, and the error is returned.
func (a *Applier) RestoreConfig(ctx context.Context, backup string) error {
	bp, err := a.Paths.backupPath(backup)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(bp) //nolint:gosec // our own backup file
	if errors.Is(err, os.ErrNotExist) {
		return nil // RemoveJobs never ran here, so nothing changed
	}
	if err != nil {
		return err
	}
	if err := writeFile(a.Paths.MainConfig, b, 0o644); err != nil {
		return err
	}
	if out, err := a.Run(ctx, "zrepl", "configcheck"); err != nil {
		return fmt.Errorf("restored the original configuration, but zrepl rejects it: %w %s", err, strings.TrimSpace(string(out)))
	}
	if _, err := a.Run(ctx, "systemctl", "restart", "zrepl"); err != nil {
		return fmt.Errorf("restart zrepl: %w", err)
	}
	return a.ensureRunning(ctx)
}

// ReleaseJobs releases the holds and bookmarks of removed jobs and returns
// what was released.
func (a *Applier) ReleaseJobs(ctx context.Context, jobs []string) ([]string, error) {
	var out []string
	for _, j := range jobs {
		if !jobName.MatchString(j) {
			return out, fmt.Errorf("invalid job name %q", j)
		}
		b, err := a.Run(ctx, "zrepl", "zfs-abstraction", "release-all", "--job", j)
		out = append(out, lines(b)...)
		if err != nil {
			return out, fmt.Errorf("release %s: %w", j, err)
		}
	}
	return out, nil
}

func lines(b []byte) []string {
	var out []string
	for l := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}
