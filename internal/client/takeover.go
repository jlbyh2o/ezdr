package client

import (
	"context"
	"log/slog"

	"connectrpc.com/connect"

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
)

// takeoverAction performs one step of taking over an existing zrepl setup
// and acknowledges it with the result.
func (a *applier) takeoverAction(ctx context.Context, act *clientv1.Action) {
	// A step half done because the command stream reconnected would be worse
	// than one that finishes, so it isn't canceled with the stream.
	ctx = context.WithoutCancel(ctx)
	a.zreplMu.Lock()
	defer a.zreplMu.Unlock()
	ack := &clientv1.AckActionRequest{ActionId: act.Id}
	var err error
	switch k := act.Kind.(type) {
	case *clientv1.Action_ZreplPreflight:
		ack.Preflight, err = a.zrepl.Preflight(ctx, k.ZreplPreflight.Datasets, k.ZreplPreflight.ReleaseJob)
	case *clientv1.Action_ZreplUpgrade:
		var v string
		if v, err = a.zrepl.Upgrade(ctx); v != "" {
			ack.Output = []string{"zrepl " + v}
		}
	case *clientv1.Action_ZreplRemoveJobs:
		err = a.zrepl.RemoveJobs(ctx, k.ZreplRemoveJobs.Jobs, k.ZreplRemoveJobs.Backup)
	case *clientv1.Action_ZreplRestoreConfig:
		err = a.zrepl.RestoreConfig(ctx, k.ZreplRestoreConfig.Backup)
	case *clientv1.Action_ZreplReleaseJobs:
		ack.Output, err = a.zrepl.ReleaseJobs(ctx, k.ZreplReleaseJobs.Jobs)
	}
	ack.Succeeded = err == nil
	if err != nil {
		ack.Message = err.Error()
		slog.Error("takeover action failed", "action", act.Id, "err", err)
	} else {
		slog.Info("takeover action completed", "action", act.Id)
	}
	if _, err := a.api.AckAction(ctx, connect.NewRequest(ack)); err != nil {
		slog.Warn("acknowledge action", "action", act.Id, "err", err)
	}
	// An upgrade changes the version the portal shows.
	if _, ok := act.Kind.(*clientv1.Action_ZreplUpgrade); ok {
		a.report(ctx)
	}
}

// testAction performs one test failover step and acknowledges it with the
// result.
func (a *applier) testAction(ctx context.Context, act *clientv1.Action) {
	ctx = context.WithoutCancel(ctx)
	a.testMu.Lock()
	defer a.testMu.Unlock()
	ack := &clientv1.AckActionRequest{ActionId: act.Id}
	var err error
	switch k := act.Kind.(type) {
	case *clientv1.Action_TestOptions:
		ack.TestOptions, err = a.tests.Options(ctx, k.TestOptions.Replicas, k.TestOptions.SnapshotPrefix)
	case *clientv1.Action_TestPrepare:
		ack.Output, err = a.tests.Prepare(ctx, k.TestPrepare)
	case *clientv1.Action_TestStartGuest:
		err = a.tests.StartGuest(ctx, k.TestStartGuest.TestId, k.TestStartGuest.Type, k.TestStartGuest.TestVmid)
	case *clientv1.Action_TestCheckGuest:
		ack.GuestCheck, err = a.tests.CheckGuest(ctx, k.TestCheckGuest.TestId, k.TestCheckGuest.Type, k.TestCheckGuest.TestVmid)
	case *clientv1.Action_TestCleanup:
		ack.Output, err = a.tests.Cleanup(ctx, k.TestCleanup)
	}
	ack.Succeeded = err == nil
	if err != nil {
		ack.Message = err.Error()
		slog.Error("test failover action failed", "action", act.Id, "err", err)
	}
	if _, err := a.api.AckAction(ctx, connect.NewRequest(ack)); err != nil {
		slog.Warn("acknowledge action", "action", act.Id, "err", err)
	}
}

// failoverAction performs one failover step and acknowledges it.
func (a *applier) failoverAction(ctx context.Context, act *clientv1.Action) {
	ctx = context.WithoutCancel(ctx)
	a.failoverMu.Lock()
	defer a.failoverMu.Unlock()
	ack := &clientv1.AckActionRequest{ActionId: act.Id}
	var err error
	switch k := act.Kind.(type) {
	case *clientv1.Action_FailoverStopGuests:
		m := k.FailoverStopGuests
		ack.Output, err = a.failover.StopAndLock(ctx, m.Vmids, m.ShutdownTimeoutSeconds)
	case *clientv1.Action_FailoverSnapshot:
		err = a.failover.Snapshot(ctx, k.FailoverSnapshot.Datasets, k.FailoverSnapshot.Snapshot)
	case *clientv1.Action_FailoverReplicate:
		err = a.failover.Replicate(ctx, k.FailoverReplicate.PullJobs)
	case *clientv1.Action_FailoverPrepare:
		ack.Output, err = a.failover.Prepare(ctx, k.FailoverPrepare.PlanId, k.FailoverPrepare.Vmids)
	case *clientv1.Action_FailoverStartGuest:
		err = a.failover.StartGuest(ctx, k.FailoverStartGuest.PlanId, k.FailoverStartGuest.Vmid)
	case *clientv1.Action_FailoverCheckGuest:
		ack.GuestCheck, err = a.failover.CheckGuest(ctx, k.FailoverCheckGuest.PlanId, k.FailoverCheckGuest.Vmid)
	case *clientv1.Action_FailoverUnlockGuests:
		ack.Output, err = a.failover.Unlock(ctx, k.FailoverUnlockGuests.Vmids, k.FailoverUnlockGuests.Start)
	}
	ack.Succeeded = err == nil
	if err != nil {
		ack.Message = err.Error()
		slog.Error("failover action failed", "action", act.Id, "err", err)
	} else {
		slog.Info("failover action completed", "action", act.Id)
	}
	if _, err := a.api.AckAction(ctx, connect.NewRequest(ack)); err != nil {
		slog.Warn("acknowledge action", "action", act.Id, "err", err)
	}
}

// failbackAction runs one side of a failback round and acknowledges it.
func (a *applier) failbackAction(ctx context.Context, act *clientv1.Action) {
	ctx = context.WithoutCancel(ctx)
	a.failbackMu.Lock()
	defer a.failbackMu.Unlock()
	ack := &clientv1.AckActionRequest{ActionId: act.Id}
	var err error
	switch k := act.Kind.(type) {
	case *clientv1.Action_FailbackReceive:
		ack.Transfer, err = a.failback.Receive(ctx, k.FailbackReceive)
	case *clientv1.Action_FailbackSend:
		ack.Transfer, err = a.failback.Send(ctx, k.FailbackSend)
	}
	ack.Succeeded = err == nil
	if err != nil {
		ack.Message = err.Error()
		slog.Error("failback action failed", "action", act.Id, "err", err)
	} else {
		slog.Info("failback action completed", "action", act.Id)
	}
	if _, err := a.api.AckAction(ctx, connect.NewRequest(ack)); err != nil {
		slog.Warn("acknowledge action", "action", act.Id, "err", err)
	}
}
