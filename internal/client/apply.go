package client

import (
	"context"
	"log/slog"
	"sync"

	"connectrpc.com/connect"

	"github.com/jlbyh2o/ezdr/internal/client/zrepl"
	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
	"github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1/clientv1connect"
	"github.com/jlbyh2o/ezdr/internal/version"
)

// applier applies desired state in the background, so slow steps (such as
// installing zrepl) don't block the command stream. If newer desired state
// arrives while applying, only the newest is applied next.
type applier struct {
	api       clientv1connect.ClientServiceClient
	zrepl     *zrepl.Applier
	cert      string
	mu        sync.Mutex
	pending   *clientv1.DesiredState
	wake      chan struct{}
	lastGen   uint64
	lastError string
}

func newApplier(api clientv1connect.ClientServiceClient, cert string) *applier {
	return &applier{api: api, zrepl: zrepl.NewApplier(), cert: cert, wake: make(chan struct{}, 1)}
}

// submit queues desired state, replacing anything not yet started.
func (a *applier) submit(ds *clientv1.DesiredState) {
	a.mu.Lock()
	a.pending = ds
	a.mu.Unlock()
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// run applies queued desired state until ctx is canceled.
func (a *applier) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.wake:
		}
		a.mu.Lock()
		ds := a.pending
		a.pending = nil
		a.mu.Unlock()
		if ds == nil {
			continue
		}
		a.mu.Lock()
		done := ds.Generation == a.lastGen && a.lastError == ""
		a.mu.Unlock()
		if done {
			continue // already applied (the portal may resend on reconnects)
		}
		errText := ""
		if err := a.zrepl.Apply(ctx, ds.GetZrepl()); err != nil {
			if ctx.Err() != nil {
				return
			}
			errText = err.Error()
			slog.Error("apply configuration", "generation", ds.Generation, "err", err)
		} else {
			slog.Info("configuration applied", "generation", ds.Generation,
				"source_jobs", len(ds.GetZrepl().GetSourceJobs()), "pull_jobs", len(ds.GetZrepl().GetPullJobs()))
		}
		a.mu.Lock()
		a.lastGen, a.lastError = ds.Generation, errText
		a.mu.Unlock()
		a.report(ctx)
	}
}

// report sends the client's status: the last applied generation, any error,
// its zrepl certificate, and the zrepl version.
func (a *applier) report(ctx context.Context) {
	a.mu.Lock()
	req := &clientv1.ReportStatusRequest{
		ClientVersion: version.Version, AppliedGeneration: a.lastGen, ApplyError: a.lastError,
		ZreplCertificate: a.cert,
	}
	a.mu.Unlock()
	req.ZreplVersion = a.zrepl.Version(ctx)
	if _, err := a.api.ReportStatus(ctx, connect.NewRequest(req)); err != nil && ctx.Err() == nil {
		slog.Warn("report status", "err", err)
	}
}
