package api

import (
	"context"
	"crypto/sha256"
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

// replicationHosts loads the hosts the plans refer to.
func (d *Deps) replicationHosts(ctx context.Context, plans []replication.Plan) (map[string]*replication.Host, error) {
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
			hosts[id] = &replication.Host{ID: id, Hostname: ph.Hostname, Inventory: ph.Inventory, Certificate: h.ZreplCertificate}
		}
	}
	return hosts, nil
}

// desiredZrepl computes a host's zrepl desired state for the given plans.
func (d *Deps) desiredZrepl(ctx context.Context, hostID string, plans []replication.Plan) (*clientv1.Zrepl, []string, error) {
	hosts, err := d.replicationHosts(ctx, plans)
	if err != nil {
		return nil, nil, err
	}
	z, problems := replication.Desired(hostID, plans, hosts)
	return z, problems, nil
}

// desiredState computes a host's current desired state and its generation.
func (d *Deps) desiredState(ctx context.Context, hostID string) (*clientv1.DesiredState, error) {
	plans, err := d.activePlans(ctx, nil)
	if err != nil {
		return nil, err
	}
	z, problems, err := d.desiredZrepl(ctx, hostID, plans)
	if err != nil {
		return nil, err
	}
	for _, p := range problems {
		slog.Warn("desired state incomplete", "host", hostID, "problem", p)
	}
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(z)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	gen, err := d.Store.SetDesiredHash(ctx, hostID, sum[:])
	if err != nil {
		return nil, err
	}
	return &clientv1.DesiredState{Generation: gen, Zrepl: z}, nil
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
