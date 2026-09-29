package client

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/jlbyh2o/ezdr/internal/client/cleanup"
	"github.com/jlbyh2o/ezdr/internal/client/failback"
	"github.com/jlbyh2o/ezdr/internal/client/failover"
	"github.com/jlbyh2o/ezdr/internal/client/guests"
	"github.com/jlbyh2o/ezdr/internal/client/testfailover"
	"github.com/jlbyh2o/ezdr/internal/client/zrepl"
	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
	"github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1/clientv1connect"
	"github.com/jlbyh2o/ezdr/internal/replication"
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
	// testMu serializes test failover actions, and failoverMu failover
	// actions.
	testMu     sync.Mutex
	failoverMu sync.Mutex
	tests      *testfailover.Runner
	pending    *clientv1.DesiredState
	wake       chan struct{}
	lastGen    uint64
	lastError  string
	// current is the zrepl configuration last applied successfully.
	current *clientv1.Zrepl
	// reportVMIDs are the guests whose configurations the portal wants.
	reportVMIDs []uint32
	failover    *failover.Runner
	// failbackMu serializes failback transfers, which can run for hours, so
	// they never hold up failover actions.
	failbackMu sync.Mutex
	failback   *failback.Transfer
	// cleanupMu serializes data scans and cleanups.
	cleanupMu sync.Mutex
	cleanup   *cleanup.Runner
	// guestEvents are lock enforcement events not yet reported.
	guestEvents []string
}

func newApplier(api clientv1connect.ClientServiceClient, cert string, siteKey wgtypes.Key) *applier {
	return &applier{api: api, zrepl: zrepl.NewApplier(), tests: testfailover.NewRunner(), failover: failover.NewRunner(),
		failback: failback.NewTransfer(), cleanup: cleanup.NewRunner(), cert: cert, siteKey: siteKey,
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
		z := a.withoutBreakGlass(ds)
		if err == nil {
			a.zreplMu.Lock()
			err = a.zrepl.Apply(ctx, z)
			a.zreplMu.Unlock()
		}
		if err == nil {
			err = guests.Store(guests.DefaultPaths, ds.GetPlanGuestConfigs(), ds.GetPlanRecovery())
		}
		if err == nil {
			err = a.enforceLocks(ctx, ds.GetLockedGuests())
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
			a.current = z
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
		ZreplCertificate: a.cert, SitePublicKey: pub[:], GuestEvents: a.guestEvents,
	}
	a.mu.Unlock()
	req.ZreplVersion = a.zrepl.Version(ctx)
	if markers, err := a.failover.Markers(); err == nil {
		for _, m := range markers {
			req.BreakGlass = append(req.BreakGlass, &clientv1.BreakGlassFailover{PlanId: m.PlanID, User: m.User,
				At: timestamppb.New(m.At), Started: m.Started})
		}
	}
	if _, err := a.api.ReportStatus(ctx, connect.NewRequest(req)); err != nil {
		if ctx.Err() == nil {
			slog.Warn("report status", "err", err)
		}
	} else if len(req.GuestEvents) > 0 {
		a.mu.Lock()
		a.guestEvents = a.guestEvents[len(req.GuestEvents):]
		a.mu.Unlock()
	}
}

// Replication status is checked every statusPoll (statusActivePoll while a
// transfer runs) and reported while transfers run, when one ends, and at
// least every statusInterval, so the portal can show transfers as they
// happen.
const (
	statusInterval   = time.Minute
	statusPoll       = 15 * time.Second
	statusActivePoll = 5 * time.Second
)

// statusLoop reports the replication status of the applied pull jobs until
// ctx is canceled.
func (a *applier) statusLoop(ctx context.Context) {
	var lastReport time.Time
	wasActive := false
	for {
		a.mu.Lock()
		z := a.current
		a.mu.Unlock()
		active := false
		if len(z.GetPullJobs()) > 0 {
			st := a.zrepl.Status(ctx, z)
			active = slices.ContainsFunc(st.Jobs, replication.Transferring)
			if active || wasActive || time.Since(lastReport) >= statusInterval {
				if _, err := a.api.ReportReplication(ctx, connect.NewRequest(st)); err != nil && ctx.Err() == nil {
					slog.Warn("report replication status", "err", err)
				} else {
					lastReport = time.Now()
				}
			}
		}
		wasActive = active
		wait := statusPoll
		if active {
			wait = statusActivePoll
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// breakGlassLoop reports break-glass failovers run on the command line
// (another process) soon after they happen, and then every statusInterval
// until the portal has recorded them and the markers are gone.
func (a *applier) breakGlassLoop(ctx context.Context) {
	var lastReport time.Time
	for {
		if markers, err := a.failover.Markers(); err == nil && len(markers) > 0 && time.Since(lastReport) >= statusInterval {
			a.report(ctx)
			lastReport = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(statusPoll):
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

// enforceLocks keeps failed-over plans' guests stopped and locked on the
// primary (split-brain prevention), remembering what it did for the next
// status report.
func (a *applier) enforceLocks(ctx context.Context, locked []*clientv1.LockedGuests) error {
	var errs []error
	for _, l := range locked {
		events, err := a.failover.StopAndLock(ctx, l.Vmids, l.ShutdownTimeoutSeconds)
		for _, e := range events {
			slog.Warn("failed-over guest", "plan", l.PlanId, "event", e)
		}
		a.mu.Lock()
		a.guestEvents = append(a.guestEvents, events...)
		a.mu.Unlock()
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// withoutBreakGlass returns the desired zrepl configuration without the jobs
// of plans failed over on this host's command line (break-glass), which the
// portal may not know about yet. Markers of failovers the portal has
// recorded are removed.
func (a *applier) withoutBreakGlass(ds *clientv1.DesiredState) *clientv1.Zrepl {
	z := ds.GetZrepl()
	markers, err := a.failover.Markers()
	if err != nil {
		slog.Warn("read break-glass markers", "err", err)
		return z
	}
	var prefixes []string
	for _, m := range markers {
		if slices.Contains(ds.GetFailedOverPlans(), m.PlanID) {
			if err := a.failover.RemoveMarker(m.PlanID); err != nil {
				slog.Warn("remove break-glass marker", "plan", m.PlanID, "err", err)
			} else {
				slog.Info("the portal recorded the break-glass failover", "plan", m.PlanName)
			}
			continue
		}
		prefixes = append(prefixes, failover.JobPrefix(m.PlanID))
	}
	if len(prefixes) == 0 || z == nil {
		return z
	}
	skip := func(name string) bool {
		return slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(name, p) })
	}
	out := proto.CloneOf(z)
	out.SourceJobs = slices.DeleteFunc(out.SourceJobs, func(j *clientv1.SourceJob) bool { return skip(j.Name) })
	out.PullJobs = slices.DeleteFunc(out.PullJobs, func(j *clientv1.PullJob) bool { return skip(j.Name) })
	return out
}
