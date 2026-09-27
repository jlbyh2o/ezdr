package api

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"

	"google.golang.org/protobuf/proto"

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
	planv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/plan/v1"
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
