package api

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
	inventoryv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/inventory/v1"
	"github.com/jlbyh2o/ezdr/internal/portal/store"
)

// Sim simulates enrolled hosts for the development portal
// (cmd/ezdr-devportal): each simulated host holds a command stream, applies
// its desired state, keeps its own ZFS snapshots, replicates on a schedule,
// and answers every action. Nothing here runs in a real portal.
type Sim struct {
	d    *Deps
	opts SimOptions

	// invMu serializes inventory edits.
	invMu sync.Mutex

	mu sync.Mutex
	// snaps holds each host's snapshots by dataset, oldest first.
	snaps map[string]map[string][]simSnap
	// sources maps source job names to their primary host and datasets,
	// as the primaries last applied them.
	sources map[string]simSource
	// pulls holds each DR host's pull jobs.
	pulls map[string][]*simPull
	// stalled plans don't replicate (their newest snapshot is as old as
	// the value); failing plans report an error. Both are keyed by job name
	// prefix.
	stalled map[string]time.Duration
	failing map[string]string
}

// SimOptions sets the simulation's pace.
type SimOptions struct {
	// Cycle is how often each pull job replicates.
	Cycle time.Duration
	// Transfer is how long one replication takes.
	Transfer time.Duration
	// ActionDelay is how long hosts take to answer most actions.
	ActionDelay time.Duration
	// ClientVersion is reported as the hosts' client version.
	ClientVersion string
	// StateFile keeps the hosts' snapshots across restarts, if set.
	StateFile string
}

type simSnap struct {
	name string
	at   time.Time
}

type simSource struct {
	hostID   string
	datasets []string
	prefix   string
}

type simPull struct {
	job      *clientv1.PullJob
	next     time.Time
	sending  time.Time // zero when idle
	bytes    uint64    // expected bytes of the running transfer
	duration time.Duration
	// snap is the running transfer's snapshot. It's taken on the primary
	// and received on the DR host together when the transfer ends, so a
	// transfer cut short (by a failover) leaves no snapshot behind.
	snap simSnap
	// The previous attempt.
	lastStart, lastEnd time.Time
}

// NewSim returns a simulation for d's hosts, with the snapshots saved in
// opts.StateFile, if any.
func NewSim(d *Deps, opts SimOptions) *Sim {
	s := &Sim{
		d: d, opts: opts,
		snaps:   map[string]map[string][]simSnap{},
		sources: map[string]simSource{},
		pulls:   map[string][]*simPull{},
		stalled: map[string]time.Duration{}, failing: map[string]string{},
	}
	if opts.StateFile != "" {
		if b, err := os.ReadFile(opts.StateFile); err == nil {
			var saved map[string]map[string][]savedSnap
			if err := json.Unmarshal(b, &saved); err != nil {
				slog.Warn("sim: ignoring saved state", "err", err)
			}
			for host, datasets := range saved {
				s.snaps[host] = map[string][]simSnap{}
				for ds, list := range datasets {
					for _, snap := range list {
						s.snaps[host][ds] = append(s.snaps[host][ds], simSnap{name: snap.Name, at: snap.At})
					}
				}
			}
		}
	}
	return s
}

type savedSnap struct {
	Name string    `json:"name"`
	At   time.Time `json:"at"`
}

// Save writes the hosts' snapshots to the state file until ctx is done.
func (s *Sim) Save(ctx context.Context) {
	if s.opts.StateFile == "" {
		return
	}
	save := func() {
		s.mu.Lock()
		saved := map[string]map[string][]savedSnap{}
		for host, datasets := range s.snaps {
			saved[host] = map[string][]savedSnap{}
			for ds, list := range datasets {
				for _, snap := range list {
					saved[host][ds] = append(saved[host][ds], savedSnap{Name: snap.name, At: snap.at})
				}
			}
		}
		s.mu.Unlock()
		b, _ := json.Marshal(saved)
		if err := os.WriteFile(s.opts.StateFile, b, 0o600); err != nil {
			slog.Error("sim: save state", "err", err)
		}
	}
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			save()
			return
		case <-tick.C:
			save()
		}
	}
}

// Stall stops replicating the plan. Its newest snapshot is age old when the
// simulation starts, and its RPO keeps growing.
func (s *Sim) Stall(planID string, age time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stalled[jobPrefix(planID)] = age
}

// Fail makes the plan's replication report message as an error.
func (s *Sim) Fail(planID, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failing[jobPrefix(planID)] = message
}

func jobPrefix(planID string) string {
	if len(planID) > 8 {
		planID = planID[:8]
	}
	return "ezdr_" + planID + "_"
}

// Run connects hostID and serves it until ctx is done.
func (s *Sim) Run(ctx context.Context, hostID string) {
	go func() {
		for ctx.Err() == nil {
			hctx, outbox, release := s.d.Hub.connect(ctx, hostID)
			s.serve(hctx, hostID, outbox)
			release()
		}
	}()
}

func (s *Sim) serve(ctx context.Context, hostID string, outbox <-chan *clientv1.SubscribeResponse) {
	touch := func() {
		if err := s.d.Store.TouchHost(ctx, hostID, s.opts.ClientVersion); err != nil && ctx.Err() == nil {
			slog.Error("sim: touch host", "host", hostID, "err", err)
		}
	}
	touch()
	if ds, err := s.d.desiredState(ctx, hostID); err == nil {
		s.apply(ctx, hostID, ds)
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	heartbeat := time.NewTicker(HeartbeatInterval)
	defer heartbeat.Stop()
	// Report the inventory now and every few minutes, as clients do.
	s.editInventory(ctx, hostID, func(*inventoryv1.Inventory) {})
	inventory := time.NewTicker(5 * time.Minute)
	defer inventory.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-outbox:
			if ds := msg.GetDesiredState(); ds != nil {
				s.apply(ctx, hostID, ds)
			}
			if a := msg.GetAction(); a != nil {
				go s.answer(ctx, hostID, a)
			}
		case <-heartbeat.C:
			touch()
		case <-inventory.C:
			s.editInventory(ctx, hostID, func(*inventoryv1.Inventory) {})
		case now := <-tick.C:
			s.step(ctx, hostID, now)
		}
	}
}

// apply records a host's desired zrepl jobs.
func (s *Sim) apply(ctx context.Context, hostID string, ds *clientv1.DesiredState) {
	s.mu.Lock()
	for name, src := range s.sources {
		if src.hostID == hostID {
			delete(s.sources, name)
		}
	}
	for _, j := range ds.GetZrepl().GetSourceJobs() {
		s.sources[j.Name] = simSource{hostID: hostID, datasets: j.Datasets, prefix: j.SnapshotPrefix}
		age := time.Duration(j.IntervalSeconds) * time.Second / 2
		if stalled, ok := s.stalled[jobPrefixOf(j.Name)]; ok {
			age = stalled
		}
		for _, dataset := range j.Datasets {
			s.ensureSnapshot(hostID, dataset, j.SnapshotPrefix, time.Now().Add(-age))
		}
	}
	old := map[string]*simPull{}
	for _, p := range s.pulls[hostID] {
		old[p.job.Name] = p
	}
	var pulls []*simPull
	for _, j := range ds.GetZrepl().GetPullJobs() {
		p := old[j.Name]
		if p == nil {
			// Stagger the jobs across the cycle, as their schedules would be.
			p = &simPull{next: time.Now().Add(2*time.Second + simSpread(j.Name, s.opts.Cycle))}
		}
		p.job = j
		pulls = append(pulls, p)
	}
	s.pulls[hostID] = pulls
	s.mu.Unlock()
	if err := s.d.Store.SetHostApplied(ctx, hostID, ds.Generation, ""); err != nil && ctx.Err() == nil {
		slog.Error("sim: set applied", "host", hostID, "err", err)
	}
	if len(ds.ReportGuestConfigs) > 0 {
		s.reportGuestConfigs(ctx, hostID, ds.ReportGuestConfigs)
	}
}

// reportGuestConfigs reports configuration files built from the inventory.
func (s *Sim) reportGuestConfigs(ctx context.Context, hostID string, vmids []uint32) {
	inv, err := s.d.Store.HostInventory(ctx, hostID)
	if err != nil {
		return
	}
	msg := &inventoryv1.Inventory{}
	if err := proto.Unmarshal(inv.Data, msg); err != nil {
		return
	}
	var configs []store.GuestConfig
	for _, g := range msg.Guests {
		if slices.Contains(vmids, g.Vmid) {
			configs = append(configs, SimGuestConfig(g))
		}
	}
	changed, err := s.d.Store.PutGuestConfigs(ctx, hostID, configs)
	if err != nil {
		slog.Error("sim: put guest configs", "host", hostID, "err", err)
		return
	}
	if changed {
		go s.d.reconcile(context.WithoutCancel(ctx), s.d.relatedHosts(ctx, hostID)...)
	}
}

// SimGuestConfig renders a Proxmox VE configuration file for a guest.
func SimGuestConfig(g *inventoryv1.Guest) store.GuestConfig {
	var b strings.Builder
	typ := "qemu"
	if g.Type == inventoryv1.GuestType_GUEST_TYPE_CONTAINER {
		typ = "lxc"
		fmt.Fprintf(&b, "arch: amd64\ncores: %d\nhostname: %s\nmemory: %d\nostype: debian\n", g.Cores, g.Name, g.MemoryBytes>>20)
	} else {
		fmt.Fprintf(&b, "boot: order=scsi0\ncores: %d\nmemory: %d\nname: %s\nostype: l26\nscsihw: virtio-scsi-single\n",
			g.Cores, g.MemoryBytes>>20, g.Name)
	}
	for _, n := range g.Nics {
		if typ == "lxc" {
			fmt.Fprintf(&b, "%s: name=eth0,bridge=%s,hwaddr=%s,ip=dhcp,type=veth\n", n.Key, n.Bridge, n.Mac)
		} else {
			fmt.Fprintf(&b, "%s: virtio=%s,bridge=%s\n", n.Key, n.Mac, n.Bridge)
		}
	}
	if g.Onboot {
		b.WriteString("onboot: 1\n")
	}
	for _, d := range g.Disks {
		fmt.Fprintf(&b, "%s: %s:%s,size=%dG\n", d.Key, d.Storage, d.Volume, d.SizeBytes>>30)
	}
	return store.GuestConfig{VMID: g.Vmid, Type: typ, Config: b.String()}
}

// ensureSnapshot gives a dataset a first snapshot. Callers hold s.mu.
func (s *Sim) ensureSnapshot(hostID, dataset, prefix string, at time.Time) {
	if s.snaps[hostID] == nil {
		s.snaps[hostID] = map[string][]simSnap{}
	}
	if len(s.snaps[hostID][dataset]) == 0 {
		s.snaps[hostID][dataset] = []simSnap{{name: snapName(prefix, at), at: at}}
	}
}

func snapName(prefix string, at time.Time) string {
	return prefix + at.UTC().Format("20060102_150405_000")
}

// addSnapshot appends a snapshot, keeping the newest 48. Callers hold s.mu.
func (s *Sim) addSnapshot(hostID, dataset string, snap simSnap) {
	if s.snaps[hostID] == nil {
		s.snaps[hostID] = map[string][]simSnap{}
	}
	list := s.snaps[hostID][dataset]
	if n := len(list); n > 0 && list[n-1].name == snap.name {
		return
	}
	list = append(list, snap)
	if len(list) > 48 {
		list = list[len(list)-48:]
	}
	s.snaps[hostID][dataset] = list
}

func (s *Sim) latest(hostID, dataset string) (simSnap, bool) {
	list := s.snaps[hostID][dataset]
	if len(list) == 0 {
		return simSnap{}, false
	}
	return list[len(list)-1], true
}

// step advances a DR host's pull jobs and reports its replication status.
func (s *Sim) step(ctx context.Context, hostID string, now time.Time) {
	s.mu.Lock()
	if len(s.pulls[hostID]) == 0 {
		s.mu.Unlock()
		return
	}
	st := &clientv1.ReportReplicationRequest{}
	for _, p := range s.pulls[hostID] {
		src, ok := s.sources[strings.TrimSuffix(p.job.Name, "_pull")]
		_, stalled := s.stalled[jobPrefixOf(p.job.Name)]
		switch {
		case !ok:
		case stalled:
			// Replicated once, before the simulation started.
			if _, done := s.latest(hostID, p.job.ReceiveDataset+"/"+src.datasets[0]); !done && len(src.datasets) > 0 {
				s.finishPull(hostID, p, src)
			}
		case !p.sending.IsZero() && now.Sub(p.sending) >= p.duration:
			for _, dataset := range src.datasets {
				s.addSnapshot(src.hostID, dataset, p.snap)
			}
			s.finishPull(hostID, p, src)
			p.lastStart, p.lastEnd = p.sending, now
			// Jitter keeps jobs from running in lockstep.
			jitter := simSpread(p.job.Name+now.String(), s.opts.Cycle/2)
			p.sending, p.next = time.Time{}, now.Add(s.opts.Cycle+jitter)
		case p.sending.IsZero() && !now.Before(p.next):
			// The primary snapshots, then the DR host pulls.
			snap := simSnap{name: snapName(src.prefix, now), at: now}
			p.snap = snap
			h := simHash(p.job.Name + snap.name)
			p.sending, p.bytes = now, 4<<20+h%(512<<20)
			p.duration = s.opts.Transfer/2 + simSpread(p.job.Name+snap.name, s.opts.Transfer)
		}
		st.Jobs = append(st.Jobs, s.jobStatus(hostID, p, src, now))
	}
	s.mu.Unlock()
	b, _ := proto.Marshal(st)
	if err := s.d.Store.PutReplicationStatus(ctx, hostID, b); err != nil && ctx.Err() == nil {
		slog.Error("sim: report replication", "host", hostID, "err", err)
	}
}

// jobPrefixOf returns the "ezdr_<plan>_" part of a job name.
func jobPrefixOf(job string) string {
	parts := strings.SplitN(job, "_", 3)
	if len(parts) < 3 {
		return job
	}
	return parts[0] + "_" + parts[1] + "_"
}

// finishPull copies the primary's newest snapshots to the replicas. Callers
// hold s.mu.
func (s *Sim) finishPull(hostID string, p *simPull, src simSource) {
	for _, dataset := range src.datasets {
		if snap, ok := s.latest(src.hostID, dataset); ok {
			s.addSnapshot(hostID, p.job.ReceiveDataset+"/"+dataset, snap)
		}
	}
}

// jobStatus describes a pull job like `zrepl status` does. Callers hold s.mu.
func (s *Sim) jobStatus(hostID string, p *simPull, src simSource, now time.Time) *clientv1.JobStatus {
	js := &clientv1.JobStatus{Name: p.job.Name, Type: "pull", State: "done"}
	sending := !p.sending.IsZero()
	if sending {
		js.State, js.AttemptStartedAt = "fan-out-filesystems", timestamppb.New(p.sending)
	} else if !p.lastStart.IsZero() {
		js.AttemptStartedAt, js.AttemptFinishedAt = timestamppb.New(p.lastStart), timestamppb.New(p.lastEnd)
	}
	msg := s.failing[jobPrefixOf(p.job.Name)]
	for i, dataset := range src.datasets {
		ds := &clientv1.DatasetStatus{Dataset: dataset, State: "done", Error: msg}
		if snap, ok := s.latest(hostID, p.job.ReceiveDataset+"/"+dataset); ok {
			ds.LatestSnapshot, ds.LatestSnapshotAt = snap.name, timestamppb.New(snap.at)
		}
		// Only some disks change between snapshots; the first always does.
		if sending && (i == 0 || simHash(p.sending.String()+dataset)%3 != 0) {
			per := p.bytes / uint64(max(1, len(src.datasets)))
			frac := min(1, float64(now.Sub(p.sending))/float64(max(1, p.duration)))
			ds.State, ds.BytesExpected, ds.BytesReplicated = "stepping", per, uint64(float64(per)*frac)
		}
		js.Datasets = append(js.Datasets, ds)
	}
	return js
}

// simSpread returns a duration below d, the same for the same key.
func simSpread(key string, d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return time.Duration(simHash(key) % uint64(d)) //nolint:gosec // below d, so it fits
}

func simHash(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

// answer handles one action after a realistic delay and acknowledges it.
func (s *Sim) answer(ctx context.Context, hostID string, a *clientv1.Action) {
	delay := s.opts.ActionDelay
	ack := &clientv1.AckActionRequest{ActionId: a.Id, Succeeded: true}
	switch k := a.Kind.(type) {
	case *clientv1.Action_ZreplPreflight:
		ack.Preflight = &clientv1.ZreplPreflightResult{ZreplVersion: "v0.7.0", ZreplRunning: true}
		s.mu.Lock()
		for _, dataset := range k.ZreplPreflight.Datasets {
			res := &clientv1.DatasetSnapshots{Dataset: dataset, Exists: true, ReferencedBytes: 8 << 30}
			for i, snap := range s.snaps[hostID][dataset] {
				res.Snapshots = append(res.Snapshots, &clientv1.SnapshotInfo{
					Name: "@" + snap.name, Guid: simHash(snap.name), Createtxg: uint64(i + 1), WrittenBytes: 16 << 20})
			}
			ack.Preflight.Datasets = append(ack.Preflight.Datasets, res)
		}
		s.mu.Unlock()
	case *clientv1.Action_ZreplReleaseJobs:
		ack.Output = []string{"released holds and bookmarks of " + strings.Join(k.ZreplReleaseJobs.Jobs, ", ")}
	case *clientv1.Action_TestOptions:
		delay = s.opts.ActionDelay / 2
		ack.TestOptions = &clientv1.TestOptionsResult{MemoryAvailableBytes: 96 << 30}
		s.mu.Lock()
		for _, r := range k.TestOptions.Replicas {
			res := &clientv1.ReplicaSnapshots{Replica: r}
			list := s.snaps[hostID][r]
			res.Exists = len(list) > 0
			for i := len(list) - 1; i >= 0; i-- {
				res.Snapshots = append(res.Snapshots, &clientv1.TestSnapshot{Name: list[i].name, CreatedAt: timestamppb.New(list[i].at)})
			}
			ack.TestOptions.Replicas = append(ack.TestOptions.Replicas, res)
		}
		s.mu.Unlock()
	case *clientv1.Action_TestPrepare:
		delay = 3 * s.opts.ActionDelay
		ack.Output = []string{"cloned replicas", "registered test guests"}
	case *clientv1.Action_TestCheckGuest, *clientv1.Action_FailoverCheckGuest, *clientv1.Action_FailbackCheckGuest:
		ack.GuestCheck = &clientv1.TestGuestCheck{Running: true}
	case *clientv1.Action_TestCleanup:
		ack.Output = []string{"destroyed test guests", "destroyed clones"}
	case *clientv1.Action_FailoverStopGuests:
		delay = 3 * s.opts.ActionDelay
		s.editGuests(ctx, hostID, k.FailoverStopGuests.Vmids, func(g *inventoryv1.Guest) {
			g.Status, g.Lock = "stopped", "migrate"
		})
	case *clientv1.Action_FailoverUnlockGuests:
		s.editGuests(ctx, hostID, k.FailoverUnlockGuests.Vmids, func(g *inventoryv1.Guest) {
			g.Status, g.Lock = "running", ""
		})
	case *clientv1.Action_FailoverSnapshot:
		now := time.Now()
		s.mu.Lock()
		for _, dataset := range k.FailoverSnapshot.Datasets {
			s.addSnapshot(hostID, dataset, simSnap{name: k.FailoverSnapshot.Snapshot, at: now})
		}
		s.mu.Unlock()
	case *clientv1.Action_FailoverReplicate:
		delay = 2 * s.opts.ActionDelay
		s.mu.Lock()
		for _, p := range s.pulls[hostID] {
			if slices.Contains(k.FailoverReplicate.PullJobs, p.job.Name) {
				if src, ok := s.sources[strings.TrimSuffix(p.job.Name, "_pull")]; ok {
					s.finishPull(hostID, p, src)
				}
			}
		}
		s.mu.Unlock()
	case *clientv1.Action_FailbackSend:
		delay = 4 * s.opts.ActionDelay
		ack.Transfer = &clientv1.FailbackTransfer{}
		primary := s.planPrimary(ctx, k.FailbackSend.PlanId)
		s.mu.Lock()
		for _, src := range k.FailbackSend.Datasets {
			var keep []simSnap
			for _, snap := range s.snaps[primary][src.Dataset] {
				keep = append(keep, snap)
				if snap.name == src.FromSnapshot {
					break
				}
			}
			copying := false
			for _, snap := range s.snaps[hostID][src.Replica] {
				if copying {
					keep = append(keep, snap)
				}
				copying = copying || snap.name == src.FromSnapshot
				if snap.name == src.ToSnapshot {
					break
				}
			}
			if s.snaps[primary] == nil {
				s.snaps[primary] = map[string][]simSnap{}
			}
			s.snaps[primary][src.Dataset] = keep
			ack.Transfer.Datasets = append(ack.Transfer.Datasets, &clientv1.DatasetTransfer{
				Dataset: src.Dataset, Bytes: 1<<20 + simHash(src.ToSnapshot)%(64<<20)})
		}
		s.mu.Unlock()
	case *clientv1.Action_FailbackReceive:
		delay = 4 * s.opts.ActionDelay
	case *clientv1.Action_FailoverPrepare:
		delay = 2 * s.opts.ActionDelay
		s.registerGuests(ctx, hostID, k.FailoverPrepare.PlanId, k.FailoverPrepare.Vmids)
	case *clientv1.Action_FailoverStartGuest:
		s.editGuests(ctx, hostID, []uint32{k.FailoverStartGuest.Vmid}, func(g *inventoryv1.Guest) { g.Status = "running" })
	case *clientv1.Action_FailbackCleanup:
		delay = 2 * s.opts.ActionDelay
		s.editInventory(ctx, hostID, func(inv *inventoryv1.Inventory) {
			inv.Guests = slices.DeleteFunc(inv.Guests, func(g *inventoryv1.Guest) bool {
				return slices.Contains(k.FailbackCleanup.Vmids, g.Vmid)
			})
		})
	case *clientv1.Action_ZreplUpgrade:
		delay = 2 * s.opts.ActionDelay
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(delay):
	}
	s.d.Hub.deliver(hostID, ack)
}

// planPrimary returns a plan's primary host.
func (s *Sim) planPrimary(ctx context.Context, planID string) string {
	p, err := s.d.Store.PlanByID(ctx, planID)
	if err != nil {
		return ""
	}
	spec, err := decodeSpec(p.Spec)
	if err != nil {
		return ""
	}
	return spec.PrimaryHostId
}

// editGuests changes guests in a host's stored inventory.
func (s *Sim) editGuests(ctx context.Context, hostID string, vmids []uint32, fn func(*inventoryv1.Guest)) {
	s.editInventory(ctx, hostID, func(inv *inventoryv1.Inventory) {
		for _, g := range inv.Guests {
			if slices.Contains(vmids, g.Vmid) {
				fn(g)
			}
		}
	})
}

// registerGuests adds a failed-over plan's guests to the DR host's
// inventory, on the plan's recovery storages and mapped bridges, stopped.
func (s *Sim) registerGuests(ctx context.Context, hostID, planID string, vmids []uint32) {
	p, err := s.d.Store.PlanByID(ctx, planID)
	if err != nil {
		return
	}
	spec, err := decodeSpec(p.Spec)
	if err != nil {
		return
	}
	primary, err := s.inventory(ctx, spec.PrimaryHostId)
	if err != nil {
		return
	}
	bridges := map[string]string{}
	for _, m := range spec.NetworkMappings {
		bridges[m.SourceBridge] = m.TargetBridge
	}
	var add []*inventoryv1.Guest
	for _, g := range primary.Guests {
		if !slices.Contains(vmids, g.Vmid) {
			continue
		}
		c := proto.Clone(g).(*inventoryv1.Guest)
		c.Status, c.Lock, c.Tags = "stopped", "", append(c.Tags, "ezdr-failover")
		for _, d := range c.Disks {
			d.Storage = RecoveryStorageID(planID, d.Storage)
		}
		for _, n := range c.Nics {
			if b := bridges[n.Bridge]; b != "" {
				n.Bridge = b
			}
		}
		add = append(add, c)
	}
	s.editInventory(ctx, hostID, func(inv *inventoryv1.Inventory) {
		inv.Guests = slices.DeleteFunc(inv.Guests, func(g *inventoryv1.Guest) bool { return slices.Contains(vmids, g.Vmid) })
		inv.Guests = append(inv.Guests, add...)
	})
}

func (s *Sim) inventory(ctx context.Context, hostID string) (*inventoryv1.Inventory, error) {
	inv, err := s.d.Store.HostInventory(ctx, hostID)
	if err != nil {
		return nil, err
	}
	msg := &inventoryv1.Inventory{}
	return msg, proto.Unmarshal(inv.Data, msg)
}

// editInventory changes a host's stored inventory, as its next inventory
// report would.
func (s *Sim) editInventory(ctx context.Context, hostID string, fn func(*inventoryv1.Inventory)) {
	s.invMu.Lock()
	defer s.invMu.Unlock()
	msg, err := s.inventory(ctx, hostID)
	if err != nil {
		return
	}
	fn(msg)
	msg.CollectedAt = timestamppb.Now()
	data, _ := proto.Marshal(msg)
	h := fnv.New128a()
	_, _ = h.Write(data)
	notReady := 0
	for _, g := range msg.Guests {
		if !g.Ready {
			notReady++
		}
	}
	if _, err := s.d.Store.PutInventory(ctx, store.Inventory{HostID: hostID, Data: data, Hash: h.Sum(nil), CollectedAt: time.Now(),
		GuestCount: len(msg.Guests), GuestsNotReady: notReady}); err != nil {
		slog.Error("sim: put inventory", "host", hostID, "err", err)
	}
}
