package replication

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
	portalv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/portal/v1"
)

func healthInput(t *testing.T, snapAge ...time.Duration) HealthInput {
	t.Helper()
	p, hosts := fixture()
	now := time.Now()
	job := &clientv1.JobStatus{Name: "ezdr_abcdefgh_local-zfs_pull", Type: "pull", State: "done"}
	for i, ds := range []string{"rpool/subvol-101-disk-0", "rpool/subvol-102-disk-0"} {
		d := &clientv1.DatasetStatus{Dataset: ds, State: "done"}
		if i < len(snapAge) {
			d.LatestSnapshot, d.LatestSnapshotAt = "zrepl_x", timestamppb.New(now.Add(-snapAge[i]))
		}
		job.Datasets = append(job.Datasets, d)
	}
	return HealthInput{
		Plan: p, Primary: hosts["p1"], DR: hosts["d1"], PrimaryOnline: true, DROnline: true,
		DRStatus: &clientv1.ReportReplicationRequest{Jobs: []*clientv1.JobStatus{job}}, DRStatusReceivedAt: now, Now: now,
	}
}

func TestHealth(t *testing.T) {
	// Interval 300s, so the default threshold is 15 minutes.
	h, ds, _ := Health(healthInput(t, 4*time.Minute, 2*time.Minute))
	if h.State != portalv1.HealthState_HEALTH_STATE_HEALTHY || h.RpoAgeSeconds != 240 || h.RpoAlertSeconds != 900 || len(ds) != 2 {
		t.Errorf("healthy = %v", h)
	}

	if h, _, _ := Health(healthInput(t, 20*time.Minute, time.Minute)); h.State != portalv1.HealthState_HEALTH_STATE_LAGGING {
		t.Errorf("lagging = %v", h)
	}

	if h, _, _ := Health(healthInput(t, time.Minute)); h.State != portalv1.HealthState_HEALTH_STATE_SYNCING ||
		!strings.Contains(h.Message, "1 of 2") {
		t.Errorf("syncing = %v", h)
	}

	in := healthInput(t, time.Minute, time.Minute)
	in.DRStatus.Jobs[0].Datasets[1].Error = "receive failed"
	if h, _, errs := Health(in); h.State != portalv1.HealthState_HEALTH_STATE_FAILING || len(errs) != 1 {
		t.Errorf("failing = %v, %v", h, errs)
	}

	in = healthInput(t, time.Minute, time.Minute)
	in.DRStatus.Jobs = nil // zrepl isn't running the job
	in.DRStatus.Error = "read zrepl status: exit status 1"
	if h, _, _ := Health(in); h.State != portalv1.HealthState_HEALTH_STATE_FAILING {
		t.Errorf("status error = %v", h)
	}

	in = healthInput(t, time.Minute, time.Minute)
	in.DROnline = false
	if h, _, _ := Health(in); h.State != portalv1.HealthState_HEALTH_STATE_UNKNOWN {
		t.Errorf("DR offline = %v", h)
	}

	in = healthInput(t, time.Minute, time.Minute)
	in.DRStatusReceivedAt = in.Now.Add(-10 * time.Minute)
	if h, _, _ := Health(in); h.State != portalv1.HealthState_HEALTH_STATE_UNKNOWN {
		t.Errorf("stale status = %v", h)
	}

	in = healthInput(t, time.Minute, time.Minute)
	in.PrimaryOnline = false
	if h, _, _ := Health(in); h.State != portalv1.HealthState_HEALTH_STATE_HEALTHY || !strings.Contains(h.Message, "pve1 is offline") {
		t.Errorf("primary offline = %v", h)
	}

	in = healthInput(t, 20*time.Minute, time.Minute)
	in.Plan.Spec.RpoAlertSeconds = 3600
	if h, _, _ := Health(in); h.State != portalv1.HealthState_HEALTH_STATE_HEALTHY {
		t.Errorf("custom threshold = %v", h)
	}
}
