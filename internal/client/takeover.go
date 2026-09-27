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
