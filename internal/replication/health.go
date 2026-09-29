package replication

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
	portalv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/portal/v1"
	"github.com/jlbyh2o/ezdr/internal/plan"
)

// StatusStale is how old a DR host's replication status may be before the
// plan's health is unknown.
const StatusStale = 3 * time.Minute

// HealthInput is what plan health is computed from.
type HealthInput struct {
	Plan          Plan
	Primary, DR   *Host
	PrimaryOnline bool
	DROnline      bool
	// DRStatus is the DR host's latest replication status, if any.
	DRStatus           *clientv1.ReportReplicationRequest
	DRStatusReceivedAt time.Time
	Now                time.Time
}

// RPOAlertThreshold is the plan's RPO alert threshold.
func RPOAlertThreshold(spec interface {
	GetRpoAlertSeconds() uint32
	GetIntervalSeconds() uint32
}) time.Duration {
	if s := spec.GetRpoAlertSeconds(); s > 0 {
		return time.Duration(s) * time.Second
	}
	return 3 * time.Duration(spec.GetIntervalSeconds()) * time.Second
}

// Health computes an active plan's replication health from the DR host's
// status. The RPO age is the age of the oldest dataset's newest replicated
// snapshot, taken from ZFS on the DR host.
func Health(in HealthInput) (*portalv1.PlanHealth, []*portalv1.DatasetHealth, []string) {
	spec := in.Plan.Spec
	threshold := RPOAlertThreshold(spec)
	h := &portalv1.PlanHealth{RpoAlertSeconds: uint64(threshold.Seconds())}

	if in.Primary == nil || in.DR == nil || in.Primary.Inventory == nil {
		h.State, h.Message = portalv1.HealthState_HEALTH_STATE_UNKNOWN, "a host or its inventory is missing"
		return h, nil, nil
	}

	// The datasets the plan should replicate, keyed by pull job.
	expected := map[string][]string{}
	for _, g := range plan.JobGroups(spec, in.Primary.Inventory) {
		expected[PullJobName(JobName(in.Plan.ID, g))] = g.Datasets
	}

	var datasets []*portalv1.DatasetHealth
	var errs []string
	byJob := map[string]*clientv1.JobStatus{}
	if in.DRStatus != nil {
		if in.DRStatus.Error != "" {
			errs = append(errs, in.DRStatus.Error)
		}
		for _, j := range in.DRStatus.Jobs {
			byJob[j.Name] = j
		}
	}
	var oldest, newest time.Time
	missing := 0
	jobs := make([]string, 0, len(expected))
	for name := range expected {
		jobs = append(jobs, name)
	}
	sort.Strings(jobs)
	for _, name := range jobs {
		js := byJob[name]
		status := map[string]*clientv1.DatasetStatus{}
		if js != nil {
			for _, e := range js.Errors {
				if !TestClonePruningError(e) {
					errs = append(errs, e)
				}
			}
			for _, d := range js.Datasets {
				status[d.Dataset] = d
			}
		}
		for _, ds := range expected[name] {
			dh := &portalv1.DatasetHealth{Dataset: ds}
			if st := status[ds]; st != nil {
				dh.LatestSnapshot, dh.LatestSnapshotAt = st.LatestSnapshot, st.LatestSnapshotAt
				dh.State, dh.Error = st.State, st.Error
				dh.BytesExpected, dh.BytesReplicated = st.BytesExpected, st.BytesReplicated
				if st.Error != "" {
					errs = append(errs, fmt.Sprintf("%s: %s", ds, st.Error))
				}
			}
			if dh.LatestSnapshotAt == nil {
				missing++
			} else {
				at := dh.LatestSnapshotAt.AsTime()
				dh.AgeSeconds = uint64(max(0, in.Now.Sub(at)).Seconds())
				if oldest.IsZero() || at.Before(oldest) {
					oldest = at
				}
				if at.After(newest) {
					newest = at
				}
			}
			datasets = append(datasets, dh)
		}
	}
	if !newest.IsZero() {
		h.LastReplicationAt = timestamppb.New(newest)
	}
	var rpoAge time.Duration
	if !oldest.IsZero() {
		rpoAge = max(0, in.Now.Sub(oldest)).Truncate(time.Second)
		h.RpoAgeSeconds = uint64(rpoAge.Seconds())
	}

	var notes []string
	if !in.PrimaryOnline {
		notes = append(notes, in.Primary.Hostname+" is offline")
	}
	switch {
	case !in.DROnline:
		h.State, h.Message = portalv1.HealthState_HEALTH_STATE_UNKNOWN, in.DR.Hostname+" is offline"
	case in.DRStatus == nil || in.Now.Sub(in.DRStatusReceivedAt) > StatusStale:
		h.State, h.Message = portalv1.HealthState_HEALTH_STATE_UNKNOWN, "waiting for replication status from "+in.DR.Hostname
	case len(errs) > 0:
		h.State, h.Message = portalv1.HealthState_HEALTH_STATE_FAILING, errs[0]
	case missing > 0:
		h.State = portalv1.HealthState_HEALTH_STATE_SYNCING
		h.Message = fmt.Sprintf("initial replication: %d of %d dataset(s) replicated", len(datasets)-missing, len(datasets))
	case rpoAge > threshold:
		h.State = portalv1.HealthState_HEALTH_STATE_LAGGING
		h.Message = fmt.Sprintf("newest replicated snapshot is %s old (alert threshold %s)", rpoAge, threshold)
	default:
		h.State = portalv1.HealthState_HEALTH_STATE_HEALTHY
		h.Message = fmt.Sprintf("all %d dataset(s) replicated within %s", len(datasets), rpoAge)
	}
	if len(notes) > 0 {
		h.Message += "; " + strings.Join(notes, "; ")
	}
	return h, datasets, errs
}

// TestCloneDir is the dataset, on each DR pool, that holds test failover
// clones (docs/design/test-failover.md, section 4).
const TestCloneDir = "ezdr-test"

// TestClonePruningError reports whether a pruning error only says that
// snapshots couldn't be destroyed because test failover clones depend on
// them. zrepl prunes them once the test ends, so it isn't a problem.
func TestClonePruningError(msg string) bool {
	if !strings.HasPrefix(msg, "pruning ") {
		return false
	}
	failures := strings.Count(msg, "cannot destroy")
	if failures == 0 || failures != strings.Count(msg, "snapshot has dependent clones") {
		return false
	}
	clones := false
	listing := false
	for line := range strings.SplitSeq(msg, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "use '-R' to destroy the following datasets:"):
			listing = true
		case listing && (line == "" || strings.HasPrefix(line, ")")):
			listing = false
		case listing:
			if !strings.Contains(line, "/"+TestCloneDir+"/") {
				return false
			}
			clones = true
		}
	}
	return clones
}

// Transferring reports whether a pull job's latest replication attempt is
// still running. zrepl's attempt states are "planning",
// "fan-out-filesystems" (replicating), "planning-error",
// "filesystem-error", and "done".
func Transferring(js *clientv1.JobStatus) bool {
	return js.GetAttemptStartedAt() != nil && js.GetAttemptFinishedAt() == nil &&
		(js.State == "planning" || js.State == "fan-out-filesystems")
}
