package replication

import (
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
	inventoryv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/inventory/v1"
	planv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/plan/v1"
	portalv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/portal/v1"
	"github.com/jlbyh2o/ezdr/internal/plan"
)

// TakeoverDataset is a dataset a takeover replicates: its name on the
// primary and where its replica is on the DR host.
type TakeoverDataset struct {
	Primary, Replica string
}

// TakeoverDatasets lists the datasets an adopted plan replicates, with
// their replicas' names on the DR host.
func TakeoverDatasets(spec *planv1.PlanSpec, primary *inventoryv1.Inventory) []TakeoverDataset {
	var out []TakeoverDataset
	for _, g := range plan.JobGroups(spec, primary) {
		for _, d := range g.Datasets {
			out = append(out, TakeoverDataset{Primary: d, Replica: g.ReceiveDataset + "/" + d})
		}
	}
	return out
}

// SupportedZrepl reports whether a zrepl version (such as "v0.7.0") is one
// EZDR configures.
func SupportedZrepl(version string) bool {
	return strings.HasPrefix(version, "v0.7.") || version == "v0.7"
}

// Preflight works out what taking over a zrepl setup would do from both
// hosts' preflight results: the newest common snapshot of each dataset (by
// GUID; a bookmark on the primary counts), full sends, datasets that stop
// being replicated, and problems that block the takeover.
func Preflight(spec *planv1.PlanSpec, primaryInv, drInv *inventoryv1.Inventory, setup plan.ZreplSetup,
	pri, dr *clientv1.ZreplPreflightResult) *portalv1.TakeoverPreflight {
	r := &portalv1.TakeoverPreflight{
		PrimaryZreplVersion: pri.GetZreplVersion(), DrZreplVersion: dr.GetZreplVersion(),
		PrimaryReleases: pri.GetReleasePreview(), DrReleases: dr.GetReleasePreview(),
	}
	problem := func(format string, args ...any) { r.Problems = append(r.Problems, fmt.Sprintf(format, args...)) }
	warn := func(format string, args ...any) { r.Warnings = append(r.Warnings, fmt.Sprintf(format, args...)) }

	for _, h := range []struct {
		name    string
		res     *clientv1.ZreplPreflightResult
		upgrade *bool
	}{{"the primary", pri, &r.UpgradePrimary}, {"the DR host", dr, &r.UpgradeDr}} {
		switch v := h.res.GetZreplVersion(); {
		case v == "":
			problem("zrepl isn't installed on %s", h.name)
		case !SupportedZrepl(v):
			*h.upgrade = true
		}
		if !h.res.GetZreplRunning() {
			warn("zrepl isn't running on %s, so the old setup isn't replicating now", h.name)
		}
	}

	byName := func(res *clientv1.ZreplPreflightResult) map[string]*clientv1.DatasetSnapshots {
		m := map[string]*clientv1.DatasetSnapshots{}
		for _, d := range res.GetDatasets() {
			m[d.Dataset] = d
		}
		return m
	}
	priSets, drSets := byName(pri), byName(dr)
	planned := map[string]bool{}
	for _, td := range TakeoverDatasets(spec, primaryInv) {
		planned[td.Primary] = true
		p, q := priSets[td.Primary], drSets[td.Replica]
		if p == nil || q == nil {
			problem("%s wasn't checked; run the preflight again", td.Primary)
			continue
		}
		if !p.Exists {
			problem("%s no longer exists on the primary", td.Primary)
			continue
		}
		ds := &portalv1.TakeoverDataset{Dataset: td.Primary}
		r.Datasets = append(r.Datasets, ds)
		if !q.Exists {
			ds.FullSend, ds.FullSendBytes = true, p.ReferencedBytes
			continue
		}
		common, newest := commonSnapshot(p, q)
		switch {
		case common == "":
			problem("the replica %s has no snapshot in common with the primary, so zrepl can't replicate into it; rename or destroy the replica first",
				td.Replica)
		case newest != "":
			problem("the replica %s has a snapshot the primary doesn't (%s), newer than the newest common one (%s); zrepl won't replicate into it",
				td.Replica, newest, common)
		default:
			ds.CommonSnapshot = common
		}
	}

	for _, d := range primaryInv.GetZfsDatasets() {
		if !planned[d.Name] && plan.FilterIncludes(setup.Source.GetFilesystems(), d.Name) {
			r.DroppedDatasets = append(r.DroppedDatasets, d.Name)
		}
	}

	port := strconv.FormatUint(uint64(plan.BasePort(spec)), 10)
	for _, h := range []struct {
		name string
		inv  *inventoryv1.Inventory
		skip string
	}{{"primary", primaryInv, setup.Source.GetName()}, {"DR host", drInv, setup.Pull.GetName()}} {
		for _, j := range h.inv.GetZrepl().GetJobs() {
			if j.Managed || j.Name == h.skip {
				continue
			}
			r.OtherJobs = append(r.OtherJobs, fmt.Sprintf("%s: %s (%s)", h.name, j.Name, j.Type))
			if _, p, err := net.SplitHostPort(j.ListenAddress); err == nil && p == port && h.inv == primaryInv {
				problem("the hand-written job %s on the primary also listens on port %s, which the plan's job needs", j.Name, port)
			}
		}
	}
	if (r.UpgradePrimary || r.UpgradeDr) && len(r.OtherJobs) > 0 {
		warn("the other hand-written jobs will run on zrepl 0.7 after the upgrade")
	}
	return r
}

// commonSnapshot returns the replica's newest snapshot that the primary also
// has (as a snapshot or bookmark, matched by GUID), named as on the primary.
// If the replica has a newer snapshot the primary doesn't, it's returned as
// newest.
func commonSnapshot(primary, replica *clientv1.DatasetSnapshots) (common, newest string) {
	names := map[uint64]string{}
	for _, s := range primary.Snapshots {
		// Prefer the snapshot's name over a bookmark with the same GUID.
		if old, ok := names[s.Guid]; !ok || strings.HasPrefix(old, "#") {
			names[s.Guid] = s.Name
		}
	}
	snaps := slices.Clone(replica.Snapshots)
	slices.Reverse(snaps) // newest first
	for _, s := range snaps {
		if !strings.HasPrefix(s.Name, "@") {
			continue
		}
		if n, ok := names[s.Guid]; ok {
			return n, newest
		}
		if newest == "" {
			newest = s.Name
		}
	}
	return "", newest
}
