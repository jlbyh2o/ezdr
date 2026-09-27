// Package replication turns active DR plans into each host's zrepl desired
// state. See docs/design/replication.md.
package replication

import (
	"fmt"
	"net"
	"net/netip"
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
	// Site tunnel address and public key, once allocated and reported.
	SiteAddress   netip.Addr
	SitePublicKey []byte
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
	// Where the primary's source jobs listen, and where the DR host connects.
	var listenHost, connectHost string
	freebind := false
	switch n := p.Spec.GetNetwork().GetPath().(type) {
	case *planv1.ReplicationNetwork_Existing:
		connectHost = n.Existing.PrimaryAddress
	case *planv1.ReplicationNetwork_Tunnel:
		for _, h := range []*Host{primary, dr} {
			if !h.SiteAddress.IsValid() || len(h.SitePublicKey) != 32 {
				problems = append(problems, fmt.Sprintf("plan %q: waiting for %s's tunnel key", p.Name, h.Hostname))
			}
		}
		if len(problems) > 0 {
			return nil, nil, problems
		}
		// Listen only on the primary's tunnel address. freebind lets zrepl
		// bind before the tunnel interface is up.
		listenHost, connectHost, freebind = primary.SiteAddress.String(), primary.SiteAddress.String(), true
	default:
		return nil, nil, []string{fmt.Sprintf("plan %q: no replication network", p.Name)}
	}

	for _, g := range plan.JobGroups(p.Spec, primary.Inventory) {
		name := JobName(p.ID, g)
		port := strconv.FormatUint(uint64(g.Port), 10)
		sources = append(sources, &clientv1.SourceJob{
			Name: name, Datasets: g.Datasets, SnapshotPrefix: p.Spec.SnapshotPrefix,
			IntervalSeconds: p.Spec.IntervalSeconds, ListenAddress: net.JoinHostPort(listenHost, port),
			Encrypted: g.Encrypted, ListenFreebind: freebind,
			Peer: &clientv1.Peer{Name: PeerName(dr.ID), CertificatePem: dr.Certificate},
		})
		pulls = append(pulls, &clientv1.PullJob{
			Name: PullJobName(name), Address: net.JoinHostPort(connectHost, port),
			Peer:           &clientv1.Peer{Name: PeerName(primary.ID), CertificatePem: primary.Certificate},
			ReceiveDataset: g.ReceiveDataset, IntervalSeconds: p.Spec.IntervalSeconds,
			SnapshotPrefix:   p.Spec.SnapshotPrefix,
			PrimaryRetention: tiers(p.Spec.PrimaryRetention), DrRetention: tiers(p.Spec.DrRetention),
		})
	}
	return sources, pulls, nil
}

// TunnelKeepalive is the keepalive the connecting side of a site tunnel
// sends, so NAT mappings stay open.
const TunnelKeepalive = 25

// SiteTunnel returns hostID's site tunnel across all active plans that use
// EZDR tunnels, or nil if none do. Peers that aren't ready yet (no address
// or key) are left out and explained in problems.
func SiteTunnel(hostID string, plans []Plan, hosts map[string]*Host, prefix netip.Prefix) (*clientv1.SiteTunnel, []string) {
	self := hosts[hostID]
	var problems []string
	peers := map[string]*clientv1.SitePeer{}
	var listenPort uint32
	used := false
	for _, p := range plans {
		t := p.Spec.GetNetwork().GetTunnel()
		if t == nil || (p.Spec.PrimaryHostId != hostID && p.Spec.DrHostId != hostID) {
			continue
		}
		used = true
		selfIsPrimary := p.Spec.PrimaryHostId == hostID
		peerID := p.Spec.DrHostId
		if !selfIsPrimary {
			peerID = p.Spec.PrimaryHostId
		}
		peer := hosts[peerID]
		if peer == nil || !peer.SiteAddress.IsValid() || len(peer.SitePublicKey) != 32 {
			name := peerID
			if peer != nil {
				name = peer.Hostname
			}
			problems = append(problems, fmt.Sprintf("plan %q: waiting for %s's tunnel key", p.Name, name))
			continue
		}
		selfListens := (t.Listener == planv1.EzdrTunnel_LISTENER_PRIMARY) == selfIsPrimary
		sp := &clientv1.SitePeer{PublicKey: peer.SitePublicKey, Address: peer.SiteAddress.String()}
		if selfListens {
			listenPort = t.ListenPort
		} else {
			sp.Endpoint, sp.PersistentKeepaliveSeconds = t.Endpoint, TunnelKeepalive
		}
		peers[peerID] = sp
	}
	if !used || self == nil || !self.SiteAddress.IsValid() {
		if used && self != nil {
			problems = append(problems, fmt.Sprintf("waiting for a site tunnel address for %s", self.Hostname))
		}
		return nil, problems
	}
	st := &clientv1.SiteTunnel{Address: self.SiteAddress.String(), Prefix: prefix.String(), ListenPort: listenPort}
	ids := make([]string, 0, len(peers))
	for id := range peers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		st.Peers = append(st.Peers, peers[id])
	}
	return st, problems
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

// TunnelChanges describes differences between two site tunnels.
func TunnelChanges(before, after *clientv1.SiteTunnel) []string {
	describe := func(t *clientv1.SiteTunnel) map[string]string {
		out := map[string]string{}
		for _, p := range t.GetPeers() {
			if p.Endpoint != "" {
				out[p.Address] = fmt.Sprintf("connect to %s at %s", p.Address, p.Endpoint)
			} else {
				out[p.Address] = fmt.Sprintf("accept %s on UDP port %d", p.Address, t.ListenPort)
			}
		}
		return out
	}
	switch {
	case before == nil && after == nil:
		return nil
	case after == nil:
		return []string{"remove the EZDR site tunnel interface ezdr1"}
	}
	var out []string
	if before == nil || before.Address != after.Address {
		out = append(out, fmt.Sprintf("create WireGuard interface ezdr1 with address %s (range %s)", after.Address, after.Prefix))
	}
	b, a := describe(before), describe(after)
	var addrs []string
	for k := range a {
		addrs = append(addrs, k)
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			addrs = append(addrs, k)
		}
	}
	sort.Strings(addrs)
	for _, k := range addrs {
		switch {
		case b[k] == "":
			out = append(out, "tunnel peer: "+a[k])
		case a[k] == "":
			out = append(out, "remove tunnel peer "+k)
		case a[k] != b[k]:
			out = append(out, "change tunnel peer: "+a[k])
		}
	}
	return out
}
