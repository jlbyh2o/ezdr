package failover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/jlbyh2o/ezdr/internal/client/guests"
	"github.com/jlbyh2o/ezdr/internal/guestconfig"
)

// Break-glass failover (docs/design/failover.md, section 7): run on the DR
// host's command line when the portal is unreachable.

// Marker records a break-glass failover on the DR host. While it exists the
// client ignores the plan's zrepl jobs in its desired state, so a portal that
// hasn't heard of the failover can't restart replication into the now
// writable replicas.
type Marker struct {
	PlanID   string    `json:"plan_id"`
	PlanName string    `json:"plan_name"`
	User     string    `json:"user"`
	At       time.Time `json:"at"`
	// Started lists the guests that were started.
	Started []uint32 `json:"started"`
}

func (r *Runner) markerPath(planID string) string {
	return filepath.Join(r.Guests.Plans, planID, "break-glass.json")
}

// WriteMarker records a break-glass failover.
func (r *Runner) WriteMarker(m Marker) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.markerPath(m.PlanID)), 0o700); err != nil {
		return err
	}
	return os.WriteFile(r.markerPath(m.PlanID), b, 0o600)
}

// Markers returns the break-glass failovers recorded on this host.
func (r *Runner) Markers() ([]Marker, error) {
	recs, err := guests.LoadRecovery(r.Guests)
	if err != nil {
		return nil, err
	}
	var out []Marker
	for _, rec := range recs {
		b, err := os.ReadFile(r.markerPath(rec.PlanId))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var m Marker
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// RemoveMarker removes a plan's marker once the portal has recorded the
// failover.
func (r *Runner) RemoveMarker(planID string) error {
	err := os.Remove(r.markerPath(planID))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// JobPrefix is the start of a plan's zrepl job names.
func JobPrefix(planID string) string {
	if len(planID) > 8 {
		planID = planID[:8]
	}
	return "ezdr_" + planID + "_"
}

// RemovePlanJobs removes a plan's jobs from EZDR's zrepl jobs file and
// restarts zrepl, so replication into the replicas stops.
func (r *Runner) RemovePlanJobs(ctx context.Context, jobsFile, planID string) (int, error) {
	b, err := os.ReadFile(jobsFile) //nolint:gosec // EZDR's own jobs file
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return 0, err
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return 0, nil
	}
	root := doc.Content[0]
	removed := 0
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "jobs" || root.Content[i+1].Kind != yaml.SequenceNode {
			continue
		}
		jobs := root.Content[i+1]
		kept := jobs.Content[:0]
		for _, j := range jobs.Content {
			if strings.HasPrefix(jobName(j), JobPrefix(planID)) {
				removed++
				continue
			}
			kept = append(kept, j)
		}
		jobs.Content = kept
	}
	if removed == 0 {
		return 0, nil
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return 0, err
	}
	if err := os.WriteFile(jobsFile, out, 0o644); err != nil { //nolint:gosec // zrepl reads it
		return 0, err
	}
	if _, err := r.Run(ctx, "zrepl", "configcheck"); err != nil {
		_ = os.WriteFile(jobsFile, b, 0o644) //nolint:gosec // restore
		return 0, fmt.Errorf("zrepl rejected the jobs file without the plan's jobs (left unchanged): %w", err)
	}
	if _, err := r.Run(ctx, "systemctl", "restart", "zrepl"); err != nil {
		return removed, err
	}
	return removed, nil
}

// jobName returns a job mapping's name.
func jobName(n *yaml.Node) string {
	if n.Kind != yaml.MappingNode {
		return ""
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == "name" {
			return n.Content[i+1].Value
		}
	}
	return ""
}

// NewestSnapshot returns the oldest of the plan's replicas' newest
// snapshots with the prefix: how old the data an unplanned failover starts
// from is.
func (r *Runner) NewestSnapshot(ctx context.Context, planID string) (time.Time, error) {
	rec, stored, err := r.recovery(planID)
	if err != nil {
		return time.Time{}, err
	}
	storages := map[string]string{}
	for _, s := range rec.Storages {
		storages[s.SourceStorage] = s.ReceiveDataset + "/" + s.SourceDataset
	}
	var oldest time.Time
	for _, g := range rec.Guests {
		st, ok := stored[g.Vmid]
		if !ok {
			continue
		}
		for _, d := range guestconfig.Disks(st.Type, st.Config) {
			source, volname, _ := strings.Cut(d.Volume, ":")
			base, ok := storages[source]
			if !ok {
				continue
			}
			ds := base + "/" + volname[strings.LastIndex(volname, "/")+1:]
			out, err := r.Run(ctx, "zfs", "list", "-Hp", "-t", "snapshot", "-o", "name,creation", "-s", "creation", "-d", "1", ds)
			if err != nil {
				return time.Time{}, err
			}
			var newest time.Time
			for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
				name, created, ok := strings.Cut(line, "\t")
				if !ok || !strings.Contains(name, "@"+rec.SnapshotPrefix) {
					continue
				}
				if at, err := strconv.ParseInt(created, 10, 64); err == nil {
					newest = time.Unix(at, 0)
				}
			}
			if newest.IsZero() {
				return time.Time{}, fmt.Errorf("%s has no snapshots", ds)
			}
			if oldest.IsZero() || newest.Before(oldest) {
				oldest = newest
			}
		}
	}
	return oldest, nil
}
