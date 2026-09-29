package plan

import (
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	inventoryv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/inventory/v1"
	planv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/plan/v1"
)

// Host is what validation knows about one of a plan's hosts.
type Host struct {
	ID       string
	Hostname string
	// Inventory is nil if the host hasn't reported one.
	Inventory  *inventoryv1.Inventory
	ReceivedAt time.Time
}

// Context is everything Validate needs besides the plan itself.
type Context struct {
	// Primary and DR are nil when the plan names a host that doesn't exist.
	Primary, DR *Host
	// OtherPlans maps the primary's VMIDs protected by other plans to those
	// plans' names.
	OtherPlans map[uint32]string
	// UsedPorts maps zrepl ports used on the primary by other plans to those
	// plans' names.
	UsedPorts map[uint32]string
	// OtherTunnels are other applied plans that use EZDR tunnels.
	OtherTunnels []OtherTunnel
	// OtherReplicas are where other plans with the same DR host keep their
	// replicas.
	OtherReplicas []OtherReplicas
	Now           time.Time
	// FailedOver is set while the plan's guests run on the DR host (failing
	// over, failed over, or failing back): their registrations there are
	// the plan's own, not conflicts.
	FailedOver bool
	// Excluded are the primary's guests the user chose not to protect: they
	// aren't reported as unconfigured.
	Excluded map[uint32]bool
}

// OtherTunnel is another plan's tunnel, for conflict checks.
type OtherTunnel struct {
	Plan                    string
	PrimaryHostID, DRHostID string
	Tunnel                  *planv1.EzdrTunnel
}

// OtherReplicas is where another plan keeps its replicas on the DR host,
// for conflict checks.
type OtherReplicas struct {
	Plan            string
	ReceiveDatasets []string
	Paths           []string
}

// listener returns the ID of the host that accepts a tunnel's connection.
func listener(primary, dr string, t *planv1.EzdrTunnel) string {
	if t.GetListener() == planv1.EzdrTunnel_LISTENER_PRIMARY {
		return primary
	}
	return dr
}

// Validation thresholds.
const (
	staleInventory = 30 * time.Minute
	spaceHeadroom  = 1.2
)

var (
	prefixPattern  = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,32}$`)
	datasetPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]*(/[A-Za-z0-9_.:-]+)*$`)
	dnsNamePattern = regexp.MustCompile(`^(\*\.)?([A-Za-z0-9_]([A-Za-z0-9_-]{0,61}[A-Za-z0-9_])?\.)*[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.?$`)
)

// Settings sections that issues are about (planv1.Issue.Section).
const (
	SectionGeneral  = "general"
	SectionTakeover = "takeover"
	SectionGuests   = "guests"
	SectionMappings = "mappings"
	SectionNetwork  = "network"
	SectionSchedule = "schedule"
	SectionDNS      = "dns"
	SectionAdvanced = "advanced"
)

// issues collects validation issues, each about the current section.
type issues struct {
	list    []*planv1.Issue
	section string
}

// in makes the following issues about section.
func (is *issues) in(section string) { is.section = section }

func (is *issues) errorf(vmid uint32, format string, args ...any) {
	is.list = append(is.list, &planv1.Issue{Severity: planv1.Severity_SEVERITY_ERROR, Vmid: vmid,
		Message: fmt.Sprintf(format, args...), Section: is.section})
}

func (is *issues) warnf(vmid uint32, format string, args ...any) {
	is.list = append(is.list, &planv1.Issue{Severity: planv1.Severity_SEVERITY_WARNING, Vmid: vmid,
		Message: fmt.Sprintf(format, args...), Section: is.section})
}

// Validate checks a plan against its hosts' inventories. Errors block
// activation; warnings don't. Results are ordered errors first.
func Validate(spec *planv1.PlanSpec, ctx Context) []*planv1.Issue {
	var is issues
	validateGeneral(spec, ctx, &is)

	if ctx.Primary == nil || ctx.DR == nil || ctx.Primary.Inventory == nil || ctx.DR.Inventory == nil {
		return sorted(is)
	}
	primary, dr := ctx.Primary.Inventory, ctx.DR.Inventory
	is.in(SectionGeneral)
	for _, h := range []*Host{ctx.Primary, ctx.DR} {
		if age := ctx.Now.Sub(h.ReceivedAt); age > staleInventory {
			is.warnf(0, "%s's inventory is %s old; the host may be offline", h.Hostname, age.Round(time.Minute))
		}
	}

	guests := guestsByID(primary)
	drGuests := guestsByID(dr)
	protected := map[uint32]bool{}
	is.in(SectionGuests)
	for _, pg := range spec.Guests {
		protected[pg.Vmid] = true
		validateGuest(pg, guests[pg.Vmid], drGuests[pg.Vmid], ctx, &is)
	}

	usedStorage, usedBridges := used(spec, guests)
	is.in(SectionMappings)
	validateStorage(spec, usedStorage, primary, dr, guests, &is)
	validateNetwork(spec, usedBridges, dr, guests, &is)
	validateReplicaPaths(spec, primary, ctx, &is)
	is.in(SectionNetwork)
	validateReplicationNetwork(spec, primary, ctx, &is)
	is.in(SectionTakeover)
	validateTakeover(spec, primary, dr, &is)
	is.in(SectionAdvanced)
	validateTestSettings(spec, dr, &is)
	is.in(SectionGuests)

	var unprotected []string
	for _, g := range primary.Guests {
		if !protected[g.Vmid] && ctx.OtherPlans[g.Vmid] == "" && !g.Template && !ctx.Excluded[g.Vmid] {
			unprotected = append(unprotected, fmt.Sprintf("%d (%s)", g.Vmid, g.Name))
		}
	}
	if len(unprotected) > 0 {
		is.warnf(0, "%s has unconfigured guests: %s. Protect them in a plan, or mark them unprotected in Guests",
			ctx.Primary.Hostname, strings.Join(unprotected, ", "))
	}
	return sorted(is)
}

func validateGeneral(spec *planv1.PlanSpec, ctx Context, is *issues) {
	is.in(SectionGeneral)
	if strings.TrimSpace(spec.Name) == "" {
		is.errorf(0, "the plan needs a name")
	}
	switch {
	case spec.PrimaryHostId == "" || spec.DrHostId == "":
		is.errorf(0, "choose a primary host and a DR host")
	case spec.PrimaryHostId == spec.DrHostId:
		is.errorf(0, "the primary and DR hosts must be different")
	case ctx.Primary == nil || ctx.DR == nil:
		is.errorf(0, "a host in this plan no longer exists")
	default:
		for _, h := range []*Host{ctx.Primary, ctx.DR} {
			if h.Inventory == nil {
				is.errorf(0, "%s hasn't reported its inventory yet", h.Hostname)
			}
		}
	}
	is.in(SectionGuests)
	if len(spec.Guests) == 0 {
		is.errorf(0, "select at least one guest to protect")
	}
	is.in(SectionSchedule)
	if spec.IntervalSeconds < MinIntervalSeconds || spec.IntervalSeconds > MaxIntervalSeconds {
		is.errorf(0, "the snapshot interval must be between 1 minute and 24 hours")
	}
	if !prefixPattern.MatchString(spec.SnapshotPrefix) {
		is.in(SectionAdvanced)
		is.errorf(0, "the snapshot prefix must be 1-32 letters, digits, or _ . : -")
		is.in(SectionSchedule)
	}
	for _, r := range []struct {
		label string
		tiers []*planv1.RetentionTier
	}{{"primary", spec.PrimaryRetention}, {"DR host", spec.DrRetention}} {
		if len(r.tiers) == 0 {
			is.errorf(0, "the %s retention needs at least one tier", r.label)
			continue
		}
		for _, t := range r.tiers {
			if t.Count == 0 || t.PeriodSeconds == 0 {
				is.errorf(0, "every %s retention tier needs a count and a period", r.label)
			} else if !t.KeepAll && spec.IntervalSeconds > 0 && t.PeriodSeconds < spec.IntervalSeconds {
				is.warnf(0, "the %s retention tier %dx%s is shorter than the snapshot interval", r.label, t.Count, Duration(t.PeriodSeconds))
			}
		}
	}
	// Snapshots and pulls run on independent schedules, so replicated data is
	// normally up to about two intervals old.
	if spec.RpoAlertSeconds > 0 && spec.RpoAlertSeconds <= 2*spec.IntervalSeconds {
		is.warnf(0, "the RPO alert threshold (%s) is at most twice the snapshot interval (%s); "+
			"snapshots and replication run on independent schedules, so replicated data is normally up to "+
			"about two intervals old and alerts would fire during normal operation",
			Duration(spec.RpoAlertSeconds), Duration(spec.IntervalSeconds))
	}
	if len(spec.PrimaryRetention) > 0 && Span(spec.PrimaryRetention) < uint64(spec.IntervalSeconds) {
		is.errorf(0, "the primary must keep snapshots for at least one snapshot interval")
	}
}

func validateGuest(pg *planv1.PlanGuest, g, onDR *inventoryv1.Guest, ctx Context, is *issues) {
	id := pg.Vmid
	if g == nil {
		is.errorf(id, "guest %d no longer exists on %s", id, ctx.Primary.Hostname)
		return
	}
	if other := ctx.OtherPlans[id]; other != "" {
		is.errorf(id, "guest %d (%s) is already protected by plan %q", id, g.Name, other)
	}
	if onDR != nil && !ctx.FailedOver {
		is.errorf(id, "guest ID %d is already used on %s by %q", id, ctx.DR.Hostname, onDR.Name)
	}
	for _, d := range g.Disks {
		if d.Readiness == inventoryv1.Readiness_READINESS_NOT_REPLICABLE {
			is.errorf(id, "guest %d (%s) disk %s can't be replicated: %s", id, g.Name, d.Key, d.Reason)
		}
	}
	for _, w := range g.ReadinessWarnings {
		is.warnf(id, "guest %d (%s) %s", id, g.Name, w)
	}
	for _, r := range pg.DnsRecords {
		validateDNS(id, g.Name, r, is)
	}
}

func validateDNS(id uint32, name string, r *planv1.DnsRecord, is *issues) {
	prev := is.section
	is.in(SectionDNS)
	defer is.in(prev)
	label := fmt.Sprintf("guest %d (%s) DNS record %q", id, name, r.Name)
	if !dnsNamePattern.MatchString(r.Name) {
		is.errorf(id, "%s: not a valid DNS name", label)
		return
	}
	for _, v := range []struct{ which, value string }{{"production", r.ProductionValue}, {"failover", r.FailoverValue}} {
		if strings.TrimSpace(v.value) == "" {
			is.errorf(id, "%s needs a %s value", label, v.which)
			continue
		}
		switch r.Type {
		case planv1.DnsRecordType_DNS_RECORD_TYPE_A:
			if a, err := netip.ParseAddr(v.value); err != nil || !a.Is4() {
				is.errorf(id, "%s: the %s value must be an IPv4 address", label, v.which)
			}
		case planv1.DnsRecordType_DNS_RECORD_TYPE_AAAA:
			if a, err := netip.ParseAddr(v.value); err != nil || !a.Is6() {
				is.errorf(id, "%s: the %s value must be an IPv6 address", label, v.which)
			}
		case planv1.DnsRecordType_DNS_RECORD_TYPE_CNAME:
			if !dnsNamePattern.MatchString(v.value) {
				is.errorf(id, "%s: the %s value must be a DNS name", label, v.which)
			}
		case planv1.DnsRecordType_DNS_RECORD_TYPE_TXT:
		default:
			is.errorf(id, "%s: choose a record type", label)
			return
		}
	}
	if r.ProductionValue == r.FailoverValue && r.ProductionValue != "" {
		is.warnf(id, "%s has the same production and failover value", label)
	}
}

func validateStorage(spec *planv1.PlanSpec, usedStorage []string, primary, dr *inventoryv1.Inventory, guests map[uint32]*inventoryv1.Guest, is *issues) {
	mappings := map[string]*planv1.StorageMapping{}
	for _, m := range spec.StorageMappings {
		mappings[m.SourceStorage] = m
	}
	drPools := map[string]*inventoryv1.ZfsPool{}
	for _, p := range dr.ZfsPools {
		drPools[p.Name] = p
	}

	// Space needed per DR pool: the protected datasets' used space.
	datasetUsed := map[string]uint64{}
	for _, d := range primary.ZfsDatasets {
		datasetUsed[d.Name] = d.UsedBytes
	}
	needed := map[string]uint64{}

	isUsed := map[string]bool{}
	for _, src := range usedStorage {
		isUsed[src] = true
		m := mappings[src]
		if m == nil || m.ReceiveDataset == "" {
			is.errorf(0, "storage %q is used by protected guests; choose where its replicas are stored on the DR host", src)
			continue
		}
		pool, _, _ := strings.Cut(m.ReceiveDataset, "/")
		switch {
		case !datasetPattern.MatchString(m.ReceiveDataset):
			is.errorf(0, "storage %s: %q isn't a valid ZFS dataset name", src, m.ReceiveDataset)
		case drPools[pool] == nil:
			is.errorf(0, "storage %s: ZFS pool %q doesn't exist on the DR host", src, pool)
		default:
			for _, pg := range spec.Guests {
				g := guests[pg.Vmid]
				if g == nil {
					continue
				}
				for _, d := range g.Disks {
					if d.Storage == src && d.Readiness == inventoryv1.Readiness_READINESS_REPLICABLE {
						needed[pool] += datasetUsed[d.ZfsDataset]
					}
				}
			}
		}
	}
	for _, m := range spec.StorageMappings {
		if !isUsed[m.SourceStorage] {
			is.warnf(0, "storage %s → %s isn't used by any protected disk", m.SourceStorage, m.ReceiveDataset)
		}
	}
	for pool, n := range needed {
		if free := drPools[pool].FreeBytes; float64(free) < float64(n)*spaceHeadroom {
			is.warnf(0, "DR pool %q has %s free; the protected disks use %s (plus 20%% headroom recommended)",
				pool, FormatBytes(free), FormatBytes(n))
		}
	}
}

func validateNetwork(spec *planv1.PlanSpec, usedBridges []string, dr *inventoryv1.Inventory, guests map[uint32]*inventoryv1.Guest, is *issues) {
	mappings := map[string]string{}
	for _, m := range spec.NetworkMappings {
		mappings[m.SourceBridge] = m.TargetBridge
	}
	isUsed := map[string]bool{}
	for _, src := range usedBridges {
		isUsed[src] = true
		target := mappings[src]
		if target == "" {
			is.errorf(0, "bridge %q is used by protected guests but isn't mapped to a DR bridge", src)
			continue
		}
		t := findInterface(dr, target)
		if t == nil || t.Type != "bridge" {
			is.errorf(0, "network mapping %s → %s: bridge %q doesn't exist on the DR host", src, target, target)
			continue
		}
		if t.VlanAware {
			continue
		}
		for _, pg := range spec.Guests {
			g := guests[pg.Vmid]
			if g == nil {
				continue
			}
			for _, n := range g.Nics {
				if n.Bridge == src && n.VlanTag != 0 {
					is.warnf(pg.Vmid, "guest %d (%s) uses VLAN %d on %s, but DR bridge %s isn't VLAN-aware",
						pg.Vmid, g.Name, n.VlanTag, src, target)
				}
			}
		}
	}
	for _, m := range spec.NetworkMappings {
		if !isUsed[m.SourceBridge] {
			is.warnf(0, "network mapping %s → %s isn't used by any protected guest", m.SourceBridge, m.TargetBridge)
		}
	}
	switch t := findInterface(dr, spec.TestBridge); {
	case spec.TestBridge == "":
		is.warnf(0, "no test failover bridge is set; test failovers need an isolated bridge on the DR host")
	case t == nil || t.Type != "bridge":
		is.errorf(0, "test failover bridge %q doesn't exist on the DR host", spec.TestBridge)
	case len(t.BridgePorts) > 0:
		is.warnf(0, "test failover bridge %s has physical ports (%s); test guests could reach your network",
			spec.TestBridge, strings.Join(t.BridgePorts, ", "))
	}
}

// sorted orders errors before warnings, keeping each group's order.
func sorted(is issues) []*planv1.Issue {
	sort.SliceStable(is.list, func(i, j int) bool { return is.list[i].Severity < is.list[j].Severity })
	return is.list
}

// FormatBytes formats a size in binary units, such as "1.5 GiB".
func FormatBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// Counts returns the number of errors and warnings.
func Counts(is []*planv1.Issue) (errs, warns int) {
	for _, i := range is {
		if i.Severity == planv1.Severity_SEVERITY_ERROR {
			errs++
		} else {
			warns++
		}
	}
	return errs, warns
}

var hostPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)

func validateReplicationNetwork(spec *planv1.PlanSpec, primary *inventoryv1.Inventory, ctx Context, is *issues) {
	switch n := spec.GetNetwork().GetPath().(type) {
	case *planv1.ReplicationNetwork_Existing:
		addr := n.Existing.GetPrimaryAddress()
		if _, err := netip.ParseAddr(addr); err != nil && !hostPattern.MatchString(addr) {
			is.errorf(0, "enter the primary's address as seen from the DR host (an IP address or DNS name)")
		}
		if l := n.Existing.GetListenAddress(); l != "" {
			if ip, err := netip.ParseAddr(l); err != nil {
				is.errorf(0, "the primary's listen address must be an IP address, or empty to listen on all addresses")
			} else if !hasAddress(primary, ip) {
				is.warnf(0, "%s isn't an address of any of the primary's network interfaces; zrepl can't accept connections until it is", l)
			}
		}
	case *planv1.ReplicationNetwork_Tunnel:
		validateTunnel(spec, n.Tunnel, ctx, is)
	default:
		is.errorf(0, "choose how the DR host reaches the primary")
		return
	}
	groups := JobGroups(spec, primary)
	base := BasePort(spec)
	if base < 1024 || int(base)+len(groups) > 65535 {
		is.errorf(0, "the zrepl port must be between 1024 and 65535")
		return
	}
	for _, g := range groups {
		if other := ctx.UsedPorts[g.Port]; other != "" {
			is.errorf(0, "zrepl port %d on the primary is already used by plan %q", g.Port, other)
		}
	}
	if len(groups) > 1 {
		is.warnf(0, "this plan uses %d zrepl ports on the primary (%d-%d): one per storage mapping and encryption kind",
			len(groups), base, base+uint32(len(groups))-1) //nolint:gosec // few groups
	}
}

// hasAddress reports whether one of inv's network interfaces has ip.
func hasAddress(inv *inventoryv1.Inventory, ip netip.Addr) bool {
	for _, iface := range inv.GetInterfaces() {
		if p, err := netip.ParsePrefix(iface.GetCidr()); err == nil && p.Addr() == ip {
			return true
		}
	}
	return false
}

func validateTunnel(spec *planv1.PlanSpec, t *planv1.EzdrTunnel, ctx Context, is *issues) {
	if t.Listener == planv1.EzdrTunnel_LISTENER_UNSPECIFIED {
		is.errorf(0, "choose which host accepts the tunnel connection")
	}
	host, port, err := net.SplitHostPort(t.Endpoint)
	if err != nil || (net.ParseIP(host) == nil && !hostPattern.MatchString(host)) {
		is.errorf(0, "enter the listening host's public endpoint as host:port (for example, dr.example.com:51821)")
	} else if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		is.errorf(0, "the tunnel endpoint's port must be between 1 and 65535")
	}
	if t.ListenPort == 0 || t.ListenPort > 65535 {
		is.errorf(0, "the tunnel listen port must be between 1 and 65535")
	}
	// Plans between the same hosts share one tunnel, and a host listens on
	// one port for all its tunnels.
	lis := listener(spec.PrimaryHostId, spec.DrHostId, t)
	for _, o := range ctx.OtherTunnels {
		samePair := (o.PrimaryHostID == spec.PrimaryHostId && o.DRHostID == spec.DrHostId) ||
			(o.PrimaryHostID == spec.DrHostId && o.DRHostID == spec.PrimaryHostId)
		oLis := listener(o.PrimaryHostID, o.DRHostID, o.Tunnel)
		switch {
		case samePair && (oLis != lis || o.Tunnel.Endpoint != t.Endpoint || o.Tunnel.ListenPort != t.ListenPort):
			is.errorf(0, "plan %q uses a tunnel between the same hosts with different settings; plans between two hosts share one tunnel", o.Plan)
		case oLis == lis && o.Tunnel.ListenPort != t.ListenPort:
			is.errorf(0, "the listening host already accepts tunnels on port %d for plan %q; use the same port", o.Tunnel.ListenPort, o.Plan)
		}
	}
}

// validateTestSettings checks that test failover VMIDs are valid and free.
// Guests tagged as test guests don't count: they belong to a running test.
func validateTestSettings(spec *planv1.PlanSpec, dr *inventoryv1.Inventory, is *issues) {
	if t := spec.GetShutdownTimeoutSeconds(); t != 0 && (t < 10 || t > 3600) {
		is.errorf(0, "the failover shutdown timeout must be between 10 seconds and 1 hour")
	}
	if l := spec.GetTestTimeLimitSeconds(); l != 0 && (l < MinTestTimeLimit || l > MaxTestTimeLimit) {
		is.errorf(0, "the test failover time limit must be between 15 minutes and 7 days")
	}
	offset := uint64(TestVMIDOffset(spec))
	drGuests := guestsByID(dr)
	protected := map[uint64]bool{}
	for _, pg := range spec.Guests {
		protected[uint64(pg.Vmid)] = true
	}
	for _, pg := range spec.Guests {
		id := uint64(pg.Vmid) + offset
		switch {
		case id > maxVMID:
			is.errorf(pg.Vmid, "guest %d's test failover ID %d is too large; lower the test ID offset", pg.Vmid, id)
		case protected[id]:
			is.errorf(pg.Vmid, "guest %d's test failover ID %d is another protected guest's ID; change the test ID offset", pg.Vmid, id)
		default:
			if g := drGuests[uint32(id)]; g != nil && !slices.Contains(g.Tags, TestTag) { //nolint:gosec // checked against maxVMID
				is.errorf(pg.Vmid, "guest %d's test failover ID %d is already used on the DR host by %q; change the test ID offset", pg.Vmid, id, g.Name)
			}
		}
	}
}

// validateReplicaPaths reports replicas that would share a dataset with
// another plan's: equal or nested replica paths, or another plan's receive
// dataset inside one of this plan's replicas (or the other way around).
// Cleaning up one plan's data would then delete the other's.
func validateReplicaPaths(spec *planv1.PlanSpec, primary *inventoryv1.Inventory, ctx Context, is *issues) {
	paths := ReplicaPaths(spec, primary)
	var receive []string
	for _, g := range JobGroups(spec, primary) {
		receive = append(receive, g.ReceiveDataset)
	}
	for _, o := range ctx.OtherReplicas {
		shared := map[string]bool{}
		for _, p := range paths {
			for _, q := range o.Paths {
				if within(p, q) || within(q, p) {
					shared[p] = true
				}
			}
			for _, r := range o.ReceiveDatasets {
				if within(r, p) {
					shared[p] = true
				}
			}
		}
		for _, r := range receive {
			for _, q := range o.Paths {
				if within(r, q) {
					shared[r] = true
				}
			}
		}
		if len(shared) == 0 {
			continue
		}
		list := sortedKeys(shared)
		more := ""
		if len(list) > 1 {
			more = fmt.Sprintf(" (and %d more)", len(list)-1)
		}
		is.errorf(0, "replicas would share %s%s with plan %q on the DR host; choose another receive dataset", list[0], more, o.Plan)
	}
}

// within reports whether dataset a is b or inside it.
func within(a, b string) bool { return a == b || strings.HasPrefix(a, b+"/") }
