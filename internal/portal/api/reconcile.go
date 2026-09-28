package api

import (
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
	inventoryv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/inventory/v1"
	planv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/plan/v1"
	"github.com/jlbyh2o/ezdr/internal/plan"
	"github.com/jlbyh2o/ezdr/internal/portal/store"
	"github.com/jlbyh2o/ezdr/internal/replication"
)

// activePlans returns plans whose jobs should run: active (not paused) plans,
// with their applied specifications. override, if set, replaces or adds one
// plan (used to preview changes).
func (d *Deps) activePlans(ctx context.Context, override *replication.Plan) ([]replication.Plan, error) {
	plans, err := d.Store.ListPlans(ctx)
	if err != nil {
		return nil, err
	}
	var out []replication.Plan
	for _, p := range plans {
		if override != nil && p.ID == override.ID {
			continue
		}
		if p.State != store.PlanActive || p.AppliedSpec == nil {
			continue
		}
		spec := &planv1.PlanSpec{}
		if err := proto.Unmarshal(p.AppliedSpec, spec); err != nil {
			return nil, err
		}
		out = append(out, replication.Plan{ID: p.ID, Name: p.Name, Spec: spec})
	}
	if override != nil && override.Spec != nil {
		out = append(out, *override)
	}
	return out, nil
}

// replicationHosts loads the hosts the plans refer to, allocating site
// tunnel addresses for hosts in plans that use EZDR tunnels.
func (d *Deps) replicationHosts(ctx context.Context, plans []replication.Plan) (map[string]*replication.Host, error) {
	for _, p := range plans {
		if p.Spec.GetNetwork().GetTunnel() == nil || !d.SiteTunnelPrefix.IsValid() {
			continue
		}
		for _, id := range []string{p.Spec.PrimaryHostId, p.Spec.DrHostId} {
			if _, err := d.Store.AllocateSiteAddress(ctx, id, d.SiteTunnelPrefix); err != nil && !errors.Is(err, store.ErrNotFound) {
				return nil, err
			}
		}
	}
	hosts := map[string]*replication.Host{}
	ps := PlanService{Deps: d}
	for _, p := range plans {
		for _, id := range []string{p.Spec.PrimaryHostId, p.Spec.DrHostId} {
			if _, done := hosts[id]; done {
				continue
			}
			ph, err := ps.loadHost(ctx, id)
			if err != nil {
				return nil, err
			}
			if ph == nil {
				continue
			}
			h, err := d.Store.HostByID(ctx, id)
			if err != nil {
				return nil, err
			}
			hosts[id] = &replication.Host{ID: id, Hostname: ph.Hostname, Inventory: ph.Inventory, Certificate: h.ZreplCertificate,
				SiteAddress: h.SiteAddress, SitePublicKey: h.SitePublicKey}
		}
	}
	return hosts, nil
}

// desiredConfig computes a host's zrepl jobs and site tunnel for the given
// plans.
func (d *Deps) desiredConfig(ctx context.Context, hostID string, plans []replication.Plan) (*clientv1.DesiredState, []string, error) {
	hosts, err := d.replicationHosts(ctx, plans)
	if err != nil {
		return nil, nil, err
	}
	z, problems := replication.Desired(hostID, plans, hosts)
	tunnel, tp := replication.SiteTunnel(hostID, plans, hosts, d.SiteTunnelPrefix)
	return &clientv1.DesiredState{Zrepl: z, SiteTunnel: tunnel}, append(problems, tp...), nil
}

// desiredState computes a host's current desired state and its generation.
func (d *Deps) desiredState(ctx context.Context, hostID string) (*clientv1.DesiredState, error) {
	plans, err := d.activePlans(ctx, nil)
	if err != nil {
		return nil, err
	}
	// A takeover adds its plan's jobs host by host (see takeover.go).
	taking, err := d.takeoverPlans(ctx, hostID)
	if err != nil {
		return nil, err
	}
	ds, problems, err := d.desiredConfig(ctx, hostID, append(plans, taking...))
	if err != nil {
		return nil, err
	}
	for _, p := range problems {
		slog.Warn("desired state incomplete", "host", hostID, "problem", p)
	}
	st, err := d.planHostState(ctx, hostID)
	if err != nil {
		return nil, err
	}
	ds.ReportGuestConfigs, ds.PlanGuestConfigs, ds.PlanRecovery, ds.LockedGuests = st.report, st.configs, st.recovery, st.locked
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(ds)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	if ds.Generation, err = d.Store.SetDesiredHash(ctx, hostID, sum[:]); err != nil {
		return nil, err
	}
	return ds, nil
}

// reconcile recomputes the desired state of the given hosts and sends it to
// those that are connected.
func (d *Deps) reconcile(ctx context.Context, hostIDs ...string) {
	seen := map[string]bool{}
	for _, id := range hostIDs {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ds, err := d.desiredState(ctx, id)
		if err != nil {
			slog.Error("compute desired state", "host", id, "err", err)
			continue
		}
		d.Hub.Send(id, &clientv1.SubscribeResponse{Message: &clientv1.SubscribeResponse_DesiredState{DesiredState: ds}})
	}
}

// relatedHosts returns hostID and every host that shares a plan with it.
func (d *Deps) relatedHosts(ctx context.Context, hostID string) []string {
	ids := []string{hostID}
	plans, err := d.Store.ListPlans(ctx)
	if err != nil {
		slog.Error("list plans", "err", err)
		return ids
	}
	for _, p := range plans {
		if p.State == store.PlanDraft {
			continue
		}
		if p.PrimaryHostID == hostID {
			ids = append(ids, p.DRHostID)
		} else if p.DRHostID == hostID {
			ids = append(ids, p.PrimaryHostID)
		}
	}
	return ids
}

// planHostState is what a host's desired state carries about its plans,
// beyond zrepl jobs.
type planHostState struct {
	report   []uint32
	configs  []*clientv1.PlanGuestConfigs
	recovery []*clientv1.PlanRecovery
	locked   []*clientv1.LockedGuests
}

func (d *Deps) planHostState(ctx context.Context, hostID string) (planHostState, error) {
	var st planHostState
	plans, err := d.Store.ListPlans(ctx)
	if err != nil {
		return st, err
	}
	report := map[uint32]bool{}
	for _, p := range plans {
		if (p.State != store.PlanActive && p.State != store.PlanPaused && p.State != store.PlanFailedOver) || p.AppliedSpec == nil {
			continue
		}
		spec := &planv1.PlanSpec{}
		if err := proto.Unmarshal(p.AppliedSpec, spec); err != nil {
			return st, err
		}
		if spec.PrimaryHostId == hostID {
			vmids := make([]uint32, 0, len(spec.Guests))
			for _, g := range spec.Guests {
				report[g.Vmid] = true
				vmids = append(vmids, g.Vmid)
			}
			if p.State == store.PlanFailedOver {
				slices.Sort(vmids)
				st.locked = append(st.locked, &clientv1.LockedGuests{PlanId: p.ID, Vmids: vmids,
					ShutdownTimeoutSeconds: plan.ShutdownTimeoutSeconds(spec)})
			}
		}
		if spec.DrHostId != hostID {
			continue
		}
		primary, err := d.Store.HostByID(ctx, spec.PrimaryHostId)
		if err != nil {
			return st, err
		}
		configs, err := d.Store.GuestConfigs(ctx, spec.PrimaryHostId)
		if err != nil {
			return st, err
		}
		pc := &clientv1.PlanGuestConfigs{PlanId: p.ID, PlanName: p.Name, PrimaryHostname: primary.Hostname}
		for _, g := range spec.Guests {
			if c, ok := configs[g.Vmid]; ok {
				pc.Guests = append(pc.Guests, &clientv1.GuestConfig{Vmid: c.VMID, Type: c.Type, Config: c.Config,
					ChangedAt: timestamppb.New(c.ChangedAt)})
			}
		}
		slices.SortFunc(pc.Guests, func(a, b *clientv1.GuestConfig) int { return cmp.Compare(a.Vmid, b.Vmid) })
		st.configs = append(st.configs, pc)

		var primaryInv *inventoryv1.Inventory
		if ph, err := (PlanService{Deps: d}).loadHost(ctx, spec.PrimaryHostId); err == nil && ph != nil {
			primaryInv = ph.Inventory
		}
		st.recovery = append(st.recovery, Recovery(p.ID, p.Name, primary.Hostname, spec, primaryInv))
	}
	st.report = slices.Sorted(maps.Keys(report))
	return st, nil
}

// Recovery builds the recovery information the DR host keeps for a plan.
func Recovery(planID, planName, primaryHostname string, spec *planv1.PlanSpec, primary *inventoryv1.Inventory) *clientv1.PlanRecovery {
	r := &clientv1.PlanRecovery{PlanId: planID, PlanName: planName, PrimaryHostname: primaryHostname,
		SnapshotPrefix: spec.SnapshotPrefix}
	pools := map[string]string{}
	for _, s := range primary.GetStorages() {
		pools[s.Id] = s.ZfsPool
	}
	for _, m := range spec.StorageMappings {
		if pools[m.SourceStorage] == "" {
			continue // the primary's inventory doesn't know it (yet)
		}
		r.Storages = append(r.Storages, &clientv1.RecoveryStorage{SourceStorage: m.SourceStorage, SourceDataset: pools[m.SourceStorage],
			ReceiveDataset: m.ReceiveDataset, StorageId: RecoveryStorageID(planID, m.SourceStorage)})
	}
	for _, m := range spec.NetworkMappings {
		r.Bridges = append(r.Bridges, &clientv1.RecoveryBridge{SourceBridge: m.SourceBridge, TargetBridge: m.TargetBridge})
	}
	for _, g := range spec.Guests {
		r.Guests = append(r.Guests, &clientv1.RecoveryGuest{Vmid: g.Vmid, StartupOrder: g.StartupOrder,
			StartupDelaySeconds: g.StartupDelaySeconds})
		for _, rec := range g.DnsRecords {
			r.DnsRecords = append(r.DnsRecords, &clientv1.RecoveryDnsRecord{Vmid: g.Vmid, Name: rec.Name,
				Type: strings.TrimPrefix(rec.Type.String(), "DNS_RECORD_TYPE_"), FailoverValue: rec.FailoverValue})
		}
	}
	return r
}

var storageIDInvalid = regexp.MustCompile(`[^a-z0-9-]+`)

// RecoveryStorageID names the Proxmox storage a failover adds over a source
// storage's replicas, such as "ezdr-ksii25bd-local-zfs".
func RecoveryStorageID(planID, sourceStorage string) string {
	short := planID
	if len(short) > 8 {
		short = short[:8]
	}
	return "ezdr-" + short + "-" + strings.Trim(storageIDInvalid.ReplaceAllString(strings.ToLower(sourceStorage), "-"), "-")
}
