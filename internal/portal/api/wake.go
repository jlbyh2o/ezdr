package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
	planv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/plan/v1"
	"github.com/jlbyh2o/ezdr/internal/plan"
	"github.com/jlbyh2o/ezdr/internal/portal/store"
	"github.com/jlbyh2o/ezdr/internal/replication"
)

// pullJobs returns the names of a plan's pull jobs on its DR host.
func (d *Deps) pullJobs(ctx context.Context, planID string, spec *planv1.PlanSpec) ([]string, error) {
	primary, _, err := d.loadInventoriesFor(ctx, spec)
	if err != nil {
		return nil, err
	}
	var jobs []string
	for _, g := range plan.JobGroups(spec, primary) {
		jobs = append(jobs, replication.PullJobName(replication.JobName(planID, g)))
	}
	return jobs, nil
}

// wakePullJobs starts a plan's pull jobs now: zrepl otherwise runs a job
// first one interval after it (re)starts, which it does on every change.
func (d *Deps) wakePullJobs(ctx context.Context, planID string, spec *planv1.PlanSpec) error {
	jobs, err := d.pullJobs(ctx, planID, spec)
	if err != nil || len(jobs) == 0 {
		return err
	}
	_, err = d.request(ctx, spec.DrHostId, applyTimeout, &clientv1.Action{Kind: &clientv1.Action_FailoverReplicate{
		FailoverReplicate: &clientv1.FailoverReplicate{PullJobs: jobs}}})
	return err
}

// wakeReplication waits until an active plan's DR host applied its new
// configuration, then starts the plan's pull jobs, so replication doesn't
// wait a full interval after activating or changing a plan.
func (d *Deps) wakeReplication(ctx context.Context, planID string) {
	sp, err := d.Store.PlanByID(ctx, planID)
	if err != nil || sp.State != store.PlanActive || sp.AppliedSpec == nil {
		return
	}
	spec, err := decodeSpec(sp.AppliedSpec)
	if err != nil || !d.Hub.Online(spec.DrHostId) {
		return // an offline host starts the jobs itself when it reconnects
	}
	if err := d.awaitApplied(ctx, spec.DrHostId); err != nil {
		slog.Warn("start replication now", "plan", sp.Name, "err", err)
		return
	}
	if err := d.wakePullJobs(ctx, planID, spec); err != nil {
		slog.Warn("start replication now", "plan", sp.Name, "err", err)
	}
}

// wakePoll is how often awaitApplied checks the host.
const wakePoll = 2 * time.Second

// awaitApplied waits until a host applied its current desired state, which
// reconciling already sent it.
func (d *Deps) awaitApplied(ctx context.Context, hostID string) error {
	ds, err := d.desiredState(ctx, hostID)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(applyTimeout)
	for time.Now().Before(deadline) && d.Hub.Online(hostID) {
		h, err := d.Store.HostByID(ctx, hostID)
		if err != nil {
			return err
		}
		if h.AppliedGeneration >= ds.Generation {
			if h.ApplyError != "" {
				return errors.New(h.ApplyError)
			}
			return nil
		}
		if err := sleep(ctx, wakePoll); err != nil {
			return err
		}
	}
	return fmt.Errorf("the host didn't apply its configuration within %s", applyTimeout)
}
