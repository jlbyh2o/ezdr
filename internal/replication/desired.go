// Package replication turns active DR plans into each host's zrepl desired
// state. See docs/design/replication.md.
package replication

import (
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
	inventoryv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/inventory/v1"
	planv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/plan/v1"
	"github.com/jlbyh2o/ezdr/internal/plan"
)

// Host is what desired state needs to know about a host.
type Host struct {
	ID        string
	Hostname  string
	Inventory *inventoryv1.Inventory
	// Certificate is the host's zrepl TLS certificate (PEM), once reported.
	Certificate string
}

// Plan is an active plan: its applied specification.
type Plan struct {
	ID   string
	Name string
	Spec *planv1.PlanSpec
}

// PeerName is the zrepl TLS name (certificate common name) of a host.
func PeerName(hostID string) string { return "ezdr-" + hostID }

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9]+`)

// JobName returns the stable zrepl source job name for a plan's job group.
// zrepl embeds job names in holds and bookmarks, so they never change.
func JobName(planID string, g plan.JobGroup) string {
	id := planID
	if len(id) > 8 {
		id = id[:8]
	}
	name := "ezdr_" + id + "_" + strings.Trim(unsafeName.ReplaceAllString(g.SourceStorage, "-"), "-")
	if g.Encrypted {
		name += "_enc"
	}
	return name
}

// PullJobName is the DR host's job name for a source job.
func PullJobName(source string) string { return source + "_pull" }

// Jobs returns the job pairs for one plan. Jobs that can't be built yet (for
// example, a peer hasn't reported its certificate) are left out and
// explained in the returned problems.
func Jobs(p Plan, primary, dr *Host) (sources []*clientv1.SourceJob, pulls []*clientv1.PullJob, problems []string) {
	if primary == nil || dr == nil || primary.Inventory == nil {
		return nil, nil, []string{fmt.Sprintf("plan %q: a host or its inventory is missing", p.Name)}
	}
	for _, h := range []*Host{primary, dr} {
		if h.Certificate == "" {
			problems = append(problems, fmt.Sprintf("plan %q: waiting for %s's zrepl certificate", p.Name, h.Hostname))
		}
	}
	if len(problems) > 0 {
		return nil, nil, problems
	}
	existing := p.Spec.GetNetwork().GetExisting()
	if existing == nil {
		return nil, nil, []string{fmt.Sprintf("plan %q: only existing-network replication is supported", p.Name)}
	}

	for _, g := range plan.JobGroups(p.Spec, primary.Inventory) {
		name := JobName(p.ID, g)
		port := strconv.FormatUint(uint64(g.Port), 10)
		sources = append(sources, &clientv1.SourceJob{
			Name: name, Datasets: g.Datasets, SnapshotPrefix: p.Spec.SnapshotPrefix,
			IntervalSeconds: p.Spec.IntervalSeconds, ListenAddress: ":" + port, Encrypted: g.Encrypted,
			Peer: &clientv1.Peer{Name: PeerName(dr.ID), CertificatePem: dr.Certificate},
		})
		pulls = append(pulls, &clientv1.PullJob{
			Name: PullJobName(name), Address: net.JoinHostPort(existing.PrimaryAddress, port),
			Peer:           &clientv1.Peer{Name: PeerName(primary.ID), CertificatePem: primary.Certificate},
			ReceiveDataset: g.ReceiveDataset, IntervalSeconds: p.Spec.IntervalSeconds,
			SnapshotPrefix:   p.Spec.SnapshotPrefix,
			PrimaryRetention: tiers(p.Spec.PrimaryRetention), DrRetention: tiers(p.Spec.DrRetention),
		})
	}
	return sources, pulls, nil
}

func tiers(in []*planv1.RetentionTier) []*clientv1.RetentionTier {
	out := make([]*clientv1.RetentionTier, 0, len(in))
	for _, t := range in {
		out = append(out, &clientv1.RetentionTier{Count: t.Count, PeriodSeconds: t.PeriodSeconds, KeepAll: t.KeepAll})
	}
	return out
}

// Desired returns hostID's zrepl desired state across all active plans, and
// problems that kept some jobs from being included.
func Desired(hostID string, plans []Plan, hosts map[string]*Host) (*clientv1.Zrepl, []string) {
	z := &clientv1.Zrepl{}
	var problems []string
	for _, p := range plans {
		if p.Spec.PrimaryHostId != hostID && p.Spec.DrHostId != hostID {
			continue
		}
		sources, pulls, probs := Jobs(p, hosts[p.Spec.PrimaryHostId], hosts[p.Spec.DrHostId])
		problems = append(problems, probs...)
		if p.Spec.PrimaryHostId == hostID {
			z.SourceJobs = append(z.SourceJobs, sources...)
		} else {
			z.PullJobs = append(z.PullJobs, pulls...)
		}
	}
	sort.Slice(z.SourceJobs, func(i, j int) bool { return z.SourceJobs[i].Name < z.SourceJobs[j].Name })
	sort.Slice(z.PullJobs, func(i, j int) bool { return z.PullJobs[i].Name < z.PullJobs[j].Name })
	return z, problems
}

// Describe summarizes a host's zrepl jobs as human-readable lines, used to
// preview changes.
func Describe(z *clientv1.Zrepl) map[string]string {
	out := map[string]string{}
	for _, j := range z.GetSourceJobs() {
		kind := ""
		if j.Encrypted {
			kind = " (raw, encrypted)"
		}
		out[j.Name] = fmt.Sprintf("serve %d dataset(s)%s on %s with snapshots every %s (prefix %s): %s",
			len(j.Datasets), kind, j.ListenAddress, plan.Duration(j.IntervalSeconds), j.SnapshotPrefix,
			strings.Join(j.Datasets, ", "))
	}
	for _, j := range z.GetPullJobs() {
		out[j.Name] = fmt.Sprintf("pull from %s every %s into %s; keep %s here and %s on the primary",
			j.Address, plan.Duration(j.IntervalSeconds), j.ReceiveDataset,
			grid(j.DrRetention), grid(j.PrimaryRetention))
	}
	return out
}

func grid(in []*clientv1.RetentionTier) string {
	t := make([]*planv1.RetentionTier, 0, len(in))
	for _, x := range in {
		t = append(t, &planv1.RetentionTier{Count: x.Count, PeriodSeconds: x.PeriodSeconds, KeepAll: x.KeepAll})
	}
	return plan.Grid(t)
}

// Changes compares two desired states and lists the differences.
func Changes(before, after *clientv1.Zrepl) []string {
	b, a := Describe(before), Describe(after)
	var names []string
	for n := range a {
		names = append(names, n)
	}
	for n := range b {
		if _, ok := a[n]; !ok {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	var out []string
	for _, n := range names {
		switch {
		case b[n] == "":
			out = append(out, fmt.Sprintf("add zrepl job %s: %s", n, a[n]))
		case a[n] == "":
			out = append(out, fmt.Sprintf("remove zrepl job %s (replicas and snapshots are kept)", n))
		case a[n] != b[n]:
			out = append(out, fmt.Sprintf("change zrepl job %s: %s", n, a[n]))
		}
	}
	return out
}
