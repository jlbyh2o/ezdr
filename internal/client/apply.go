package client

import (
	"bytes"
	"context"
	"log/slog"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/jlbyh2o/ezdr/internal/client/guests"
	"github.com/jlbyh2o/ezdr/internal/client/testfailover"
	"github.com/jlbyh2o/ezdr/internal/client/zrepl"
	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
	"github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1/clientv1connect"
	"github.com/jlbyh2o/ezdr/internal/version"
)

// applier applies desired state in the background, so slow steps (such as
// installing zrepl) don't block the command stream. If newer desired state
// arrives while applying, only the newest is applied next.
type applier struct {
	api     clientv1connect.ClientServiceClient
	zrepl   *zrepl.Applier
	cert    string
	siteKey wgtypes.Key
	mu      sync.Mutex
	// zreplMu serializes changes to zrepl's configuration: applying desired
	// state and takeover actions.
	zreplMu sync.Mutex
	// testMu serializes test failover actions.
	testMu    sync.Mutex
	tests     *testfailover.Runner
	pending   *clientv1.DesiredState
	wake      chan struct{}
	lastGen   uint64
	lastError string
	// current is the zrepl configuration last applied successfully.
	current *clientv1.Zrepl
	// reportVMIDs are the guests whose configurations the portal wants.
	reportVMIDs []uint32
}

func newApplier(api clientv1connect.ClientServiceClient, cert string, siteKey wgtypes.Key) *applier {
	return &applier{api: api, zrepl: zrepl.NewApplier(), tests: testfailover.NewRunner(), cert: cert, siteKey: siteKey,
		wake: make(chan struct{}, 1)}
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
		// The tunnel comes first: zrepl's jobs may use its addresses.
		err := ApplySiteTunnel(ds.GetSiteTunnel(), a.siteKey)
		if err == nil {
			a.zreplMu.Lock()
			err = a.zrepl.Apply(ctx, ds.GetZrepl())
			a.zreplMu.Unlock()
		}
		if err == nil {
			err = guests.Store(guests.DefaultPaths, ds.GetPlanGuestConfigs())
		}
		if err != nil {
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
		a.reportVMIDs = ds.GetReportGuestConfigs()
		if errText == "" {
			a.current = ds.GetZrepl()
		}
		a.mu.Unlock()
		a.report(ctx)
	}
}

// report sends the client's status: the last applied generation, any error,
// its zrepl certificate, and the zrepl version.
func (a *applier) report(ctx context.Context) {
	a.mu.Lock()
	pub := a.siteKey.PublicKey()
	req := &clientv1.ReportStatusRequest{
		ClientVersion: version.Version, AppliedGeneration: a.lastGen, ApplyError: a.lastError,
		ZreplCertificate: a.cert, SitePublicKey: pub[:],
	}
	a.mu.Unlock()
	req.ZreplVersion = a.zrepl.Version(ctx)
	if _, err := a.api.ReportStatus(ctx, connect.NewRequest(req)); err != nil && ctx.Err() == nil {
		slog.Warn("report status", "err", err)
	}
}

// statusInterval is how often replication status is reported.
const statusInterval = time.Minute

// statusLoop reports the replication status of the applied pull jobs every
// minute until ctx is canceled.
func (a *applier) statusLoop(ctx context.Context) {
	ticker := time.NewTicker(statusInterval)
	defer ticker.Stop()
	for {
		a.mu.Lock()
		z := a.current
		a.mu.Unlock()
		if len(z.GetPullJobs()) > 0 {
			st := a.zrepl.Status(ctx, z)
			if _, err := a.api.ReportReplication(ctx, connect.NewRequest(st)); err != nil && ctx.Err() == nil {
				slog.Warn("report replication status", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// guestConfigInterval is how often guest configurations are checked for
// changes; they're reported when they change and at least every
// guestConfigRefresh.
const (
	guestConfigInterval = time.Minute
	guestConfigRefresh  = 15 * time.Minute
)

// guestConfigLoop reports the configurations of the guests the portal asks
// for until ctx is canceled.
func (a *applier) guestConfigLoop(ctx context.Context) {
	var last []byte
	var lastAt time.Time
	ticker := time.NewTicker(guestConfigInterval)
	defer ticker.Stop()
	for {
		a.mu.Lock()
		vmids := a.reportVMIDs
		a.mu.Unlock()
		configs, errs := guests.ReadAll(guests.DefaultPaths, vmids)
		for _, err := range errs {
			slog.Warn("read guest configuration", "err", err)
		}
		req := &clientv1.ReportGuestConfigsRequest{Guests: configs}
		b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(req)
		if (len(vmids) > 0 || last != nil) && (!bytes.Equal(b, last) || time.Since(lastAt) > guestConfigRefresh) {
			if _, err := a.api.ReportGuestConfigs(ctx, connect.NewRequest(req)); err != nil {
				if ctx.Err() == nil {
					slog.Warn("report guest configurations", "err", err)
				}
			} else {
				last, lastAt = b, time.Now()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
