package plan

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"

	inventoryv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/inventory/v1"
	planv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/plan/v1"
)

// ZreplSetup is an existing hand-written zrepl setup between two hosts: a
// source job on the primary and the pull job on the DR host that connects to
// it.
type ZreplSetup struct {
	Source, Pull *inventoryv1.ZreplJob
}

// ZreplSetups pairs the primary's hand-written source jobs with the DR
// host's hand-written pull jobs that connect to the same port.
func ZreplSetups(primary, dr *inventoryv1.Inventory) []ZreplSetup {
	var setups []ZreplSetup
	for _, src := range primary.GetZrepl().GetJobs() {
		if src.Managed || src.Type != "source" {
			continue
		}
		_, srcPort, err := net.SplitHostPort(src.ListenAddress)
		if err != nil {
			continue
		}
		for _, pull := range dr.GetZrepl().GetJobs() {
			if pull.Managed || pull.Type != "pull" {
				continue
			}
			if _, port, err := net.SplitHostPort(pull.ConnectAddress); err == nil && port == srcPort {
				setups = append(setups, ZreplSetup{Source: src, Pull: pull})
			}
		}
	}
	return setups
}

// FilterIncludes reports whether a zrepl filesystem filter selects dataset.
// As in zrepl, the most specific matching entry decides: an exact dataset
// beats a subtree ("pool/data<"), and a deeper subtree beats a shallower one.
func FilterIncludes(filters []*inventoryv1.ZreplFilter, dataset string) bool {
	best, include := -1, false
	for _, f := range filters {
		score := -1
		if base, ok := strings.CutSuffix(f.Pattern, "<"); ok {
			if base == "" || dataset == base || strings.HasPrefix(dataset, base+"/") {
				score = 2 * len(base)
			}
		} else if dataset == f.Pattern {
			score = 2*len(f.Pattern) + 1
		}
		if score > best {
			best, include = score, f.Include
		}
	}
	return include
}

// Adopt returns a copy of spec set up to take over the named zrepl jobs: the
// snapshot prefix, interval, retention, receive dataset, and existing-network
// settings come from the old jobs, and the plan protects the guests whose
// disks the old filter replicates. The notes describe what the plan does
// differently from the old setup.
func Adopt(spec *planv1.PlanSpec, primary, dr *inventoryv1.Inventory, sourceJob, pullJob string) (*planv1.PlanSpec, []string, error) {
	var setup *ZreplSetup
	for _, c := range ZreplSetups(primary, dr) {
		if c.Source.Name == sourceJob && c.Pull.Name == pullJob {
			setup = &c
			break
		}
	}
	if setup == nil {
		return nil, nil, fmt.Errorf("no hand-written source job %q on the primary with pull job %q on the DR host", sourceJob, pullJob)
	}
	src, pull := setup.Source, setup.Pull
	s := proto.CloneOf(spec)
	var notes []string
	note := func(format string, args ...any) { notes = append(notes, fmt.Sprintf(format, args...)) }

	s.Takeover = &planv1.ZreplTakeover{SourceJob: src.Name, PullJob: pull.Name}
	if src.SnapshotPrefix == "" {
		return nil, nil, errors.New("the source job has no snapshot prefix, so its snapshots can't be told apart from others")
	}
	s.SnapshotPrefix = src.SnapshotPrefix

	switch {
	case src.SnapshottingType != "periodic" || src.SnapshotIntervalSeconds == 0:
		note("the old job's snapshots aren't periodic (%s); the plan keeps its own interval", src.SnapshottingType)
	default:
		s.IntervalSeconds = src.SnapshotIntervalSeconds
		if pull.IntervalSeconds != 0 && pull.IntervalSeconds != src.SnapshotIntervalSeconds {
			note("the old pull job ran every %s; the plan replicates at its snapshot interval, every %s",
				Duration(pull.IntervalSeconds), Duration(src.SnapshotIntervalSeconds))
		}
	}

	if tiers, extra := retention(pull.KeepSender, s.SnapshotPrefix, true); tiers != nil {
		s.PrimaryRetention = tiers
		for _, e := range extra {
			note("primary retention: %s", e)
		}
	} else {
		note("primary retention: no grid for the prefix was found; the plan keeps its own")
	}
	if tiers, extra := retention(pull.KeepReceiver, s.SnapshotPrefix, false); tiers != nil {
		s.DrRetention = tiers
		for _, e := range extra {
			note("DR retention: %s", e)
		}
	} else {
		note("DR retention: no grid for the prefix was found; the plan keeps its own")
	}

	// Network: the DR host keeps connecting to the same address and port.
	host, port, _ := net.SplitHostPort(pull.ConnectAddress)
	p, _ := strconv.ParseUint(port, 10, 16)
	existing := &planv1.ExistingNetwork{PrimaryAddress: host, Port: uint32(p)}
	if lh, _, err := net.SplitHostPort(src.ListenAddress); err == nil && lh != "" {
		if _, err := netip.ParseAddr(lh); err == nil {
			existing.ListenAddress = lh
		} else {
			note("the old job listens on %q, which isn't an IP address; the plan listens on all addresses", lh)
		}
	}
	s.Network = &planv1.ReplicationNetwork{Path: &planv1.ReplicationNetwork_Existing{Existing: existing}}

	if f := src.Send; f != nil && (!f.Compressed || !f.LargeBlocks || !f.EmbeddedData || f.Raw) {
		note("the old job's send options differ from EZDR's (compressed, large blocks, embedded data); the first sends may fail or need a full send")
	}

	adoptGuests(s, primary, src.Filesystems, note)
	if err := adoptStorage(s, primary, dr, pull.RootFs, note); err != nil {
		return nil, nil, err
	}
	return Suggest(s, primary, dr), notes, nil
}

// adoptGuests protects exactly the guests whose replicable disks the filter
// selects, keeping the settings of guests the plan already protects.
func adoptGuests(s *planv1.PlanSpec, primary *inventoryv1.Inventory, filters []*inventoryv1.ZreplFilter, note func(string, ...any)) {
	existing := map[uint32]*planv1.PlanGuest{}
	for _, pg := range s.Guests {
		existing[pg.Vmid] = pg
	}
	s.Guests = nil
	for _, g := range primary.GetGuests() {
		if g.Template {
			continue
		}
		var in, out []string
		for _, d := range g.Disks {
			if d.Readiness != inventoryv1.Readiness_READINESS_REPLICABLE || d.ZfsDataset == "" {
				continue
			}
			if FilterIncludes(filters, d.ZfsDataset) {
				in = append(in, d.ZfsDataset)
			} else {
				out = append(out, d.ZfsDataset)
			}
		}
		if len(in) == 0 {
			continue
		}
		if len(out) > 0 {
			note("guest %d (%s): the old job skipped %s; the plan replicates every disk, so %s will need a full send",
				g.Vmid, g.Name, strings.Join(out, ", "), plural(len(out), "it", "they"))
		}
		pg := existing[g.Vmid]
		if pg == nil {
			pg = &planv1.PlanGuest{Vmid: g.Vmid}
		}
		s.Guests = append(s.Guests, pg)
	}
	var dropped []uint32
	for vmid := range existing {
		if !slices.ContainsFunc(s.Guests, func(pg *planv1.PlanGuest) bool { return pg.Vmid == vmid }) {
			dropped = append(dropped, vmid)
		}
	}
	slices.Sort(dropped)
	for _, vmid := range dropped {
		note("guest %d is no longer protected: the old job doesn't replicate its disks", vmid)
	}
}

// adoptStorage points every storage the plan uses at the old receive dataset,
// on the DR storage whose pool holds it.
func adoptStorage(s *planv1.PlanSpec, primary, dr *inventoryv1.Inventory, rootFS string, note func(string, ...any)) error {
	pool, _, _ := strings.Cut(rootFS, "/")
	target := ""
	for _, st := range dr.GetStorages() {
		if st.Type == "zfspool" && (st.ZfsPool == pool || strings.HasPrefix(st.ZfsPool, pool+"/")) {
			if target == "" || strings.HasPrefix(rootFS, st.ZfsPool+"/") {
				target = st.Id
			}
		}
	}
	if target == "" {
		return fmt.Errorf("no ZFS storage on the DR host uses pool %q, where the old job receives", pool)
	}
	storages, _ := used(s, guestsByID(primary))
	mappings := map[string]*planv1.StorageMapping{}
	for _, m := range s.StorageMappings {
		mappings[m.SourceStorage] = m
	}
	for _, src := range storages {
		m := mappings[src]
		if m == nil {
			m = &planv1.StorageMapping{SourceStorage: src}
			s.StorageMappings = append(s.StorageMappings, m)
		} else if m.ReceiveDataset != "" && m.ReceiveDataset != rootFS {
			note("storage %s: replicas are received into %s (the old job's) instead of %s", src, rootFS, m.ReceiveDataset)
		}
		m.TargetStorage, m.ReceiveDataset = target, rootFS
	}
	return nil
}

var gridTier = regexp.MustCompile(`^(\d+)x(\d+)([smhdw])(\(keep=all\))?$`)

// retention converts the old pruning rules for the prefix into tiers. It
// returns nil when no grid restricted to the prefix exists, and describes
// rules that the plan's always-on rules (not_replicated on the sender, and
// keeping snapshots without the prefix) don't cover.
func retention(rules []*inventoryv1.ZreplPruneRule, prefix string, sender bool) ([]*planv1.RetentionTier, []string) {
	var tiers []*planv1.RetentionTier
	var extra []string
	keepsOthers := false
	for _, r := range rules {
		switch {
		case r.Type == "grid" && matchesPrefix(r.Regex, prefix) && tiers == nil:
			t, err := parseGrid(r.Grid)
			if err != nil {
				extra = append(extra, fmt.Sprintf("grid %q can't be used (%v)", r.Grid, err))
				continue
			}
			tiers = t
		case r.Type == "not_replicated" && sender:
		case r.Type == "regex" && r.Negate && matchesPrefix(r.Regex, prefix):
			keepsOthers = true
		default:
			extra = append(extra, fmt.Sprintf("the %s rule isn't carried over", describeRule(r)))
		}
	}
	if !keepsOthers {
		extra = append(extra, "EZDR keeps snapshots without the prefix; the old job pruned them")
	}
	return tiers, extra
}

// matchesPrefix reports whether a pruning regex selects exactly the prefix's
// snapshots.
func matchesPrefix(regex, prefix string) bool {
	return regex == "^"+prefix || regex == "^"+regexp.QuoteMeta(prefix)
}

func describeRule(r *inventoryv1.ZreplPruneRule) string {
	switch r.Type {
	case "grid":
		return fmt.Sprintf("grid %q (regex %q)", r.Grid, r.Regex)
	case "regex":
		return fmt.Sprintf("regex %q", r.Regex)
	case "last_n":
		return fmt.Sprintf("last_n %d", r.Count)
	}
	return r.Type
}

func parseGrid(grid string) ([]*planv1.RetentionTier, error) {
	units := map[string]uint32{"s": 1, "m": 60, "h": hour, "d": day, "w": 7 * day}
	var tiers []*planv1.RetentionTier
	for part := range strings.SplitSeq(grid, "|") {
		m := gridTier.FindStringSubmatch(strings.TrimSpace(part))
		if m == nil {
			return nil, fmt.Errorf("unsupported interval %q", strings.TrimSpace(part))
		}
		count, _ := strconv.ParseUint(m[1], 10, 32)
		n, _ := strconv.ParseUint(m[2], 10, 32)
		keepAll := m[4] != ""
		if keepAll && count != 1 {
			return nil, fmt.Errorf("keep=all is only supported on a single period, not %q", strings.TrimSpace(part))
		}
		tiers = append(tiers, tier(uint32(count), uint32(n)*units[m[3]], keepAll)) //nolint:gosec // parsed as 32-bit
	}
	return tiers, nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// validateTakeover checks that an adopted setup still exists and that the
// settings incremental sends depend on still match it.
func validateTakeover(spec *planv1.PlanSpec, primary, dr *inventoryv1.Inventory, is *issues) {
	t := spec.GetTakeover()
	if t == nil {
		return
	}
	var setup *ZreplSetup
	for _, c := range ZreplSetups(primary, dr) {
		if c.Source.Name == t.SourceJob && c.Pull.Name == t.PullJob {
			setup = &c
		}
	}
	if setup == nil {
		is.errorf(0, "the adopted zrepl jobs (%s on the primary, %s on the DR host) weren't found; adopt the setup again or stop adopting it",
			t.SourceJob, t.PullJob)
		return
	}
	if spec.SnapshotPrefix != setup.Source.SnapshotPrefix {
		is.errorf(0, "the adopted setup's snapshots use the prefix %q; the plan must use it too, or incremental sends can't continue",
			setup.Source.SnapshotPrefix)
	}
	for _, m := range spec.StorageMappings {
		if m.ReceiveDataset != setup.Pull.RootFs {
			is.errorf(0, "storage mapping %s → %s: the adopted setup receives into %s; the plan must too, or every dataset needs a full send",
				m.SourceStorage, m.TargetStorage, setup.Pull.RootFs)
		}
	}

	planned := map[string]bool{}
	var fullSends []string
	for _, g := range JobGroups(spec, primary) {
		for _, d := range g.Datasets {
			planned[d] = true
			if !FilterIncludes(setup.Source.Filesystems, d) {
				fullSends = append(fullSends, d)
			}
		}
	}
	if len(fullSends) > 0 {
		is.warnf(0, "the old job doesn't replicate %s, so %s will need a full send", strings.Join(fullSends, ", "),
			plural(len(fullSends), "it", "they"))
	}
	var dropped []string
	for _, d := range primary.GetZfsDatasets() {
		if !planned[d.Name] && FilterIncludes(setup.Source.Filesystems, d.Name) {
			dropped = append(dropped, d.Name)
		}
	}
	if len(dropped) > 0 {
		is.warnf(0, "the old job also replicates %s; after the takeover %s no longer replicated (existing replicas stay on the DR host)",
			strings.Join(dropped, ", "), plural(len(dropped), "it is", "they are"))
	}
}
