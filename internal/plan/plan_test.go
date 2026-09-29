package plan

import (
	"strings"
	"testing"
	"time"

	inventoryv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/inventory/v1"
	planv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/plan/v1"
)

const gib = 1 << 30

func replicable(key, storage, volume, pool string) *inventoryv1.Disk {
	return &inventoryv1.Disk{Key: key, Storage: storage, Volume: volume, ZfsDataset: pool + "/" + volume,
		Readiness: inventoryv1.Readiness_READINESS_REPLICABLE}
}

func primaryInv() *inventoryv1.Inventory {
	return &inventoryv1.Inventory{
		Host: &inventoryv1.HostInfo{Hostname: "pve1"},
		Guests: []*inventoryv1.Guest{
			{Vmid: 101, Name: "web", Ready: true, Disks: []*inventoryv1.Disk{replicable("rootfs", "local-zfs", "subvol-101-disk-0", "rpool")},
				Nics: []*inventoryv1.Nic{{Key: "net0", Bridge: "vmbr1", VlanTag: 10}}},
			{Vmid: 201, Name: "app", Ready: true, Startup: "order=2,up=30",
				Disks:             []*inventoryv1.Disk{replicable("scsi0", "local-zfs", "vm-201-disk-0", "rpool")},
				Nics:              []*inventoryv1.Nic{{Key: "net0", Bridge: "vmbr1", VlanTag: 20}},
				ReadinessWarnings: []string{"uses PCI passthrough"}},
			{Vmid: 202, Name: "legacy", Disks: []*inventoryv1.Disk{{Key: "scsi0", Storage: "local-lvm",
				Readiness: inventoryv1.Readiness_READINESS_NOT_REPLICABLE, Reason: "on lvmthin storage"}}},
			{Vmid: 300, Name: "tmpl", Template: true, Ready: true},
		},
		Storages: []*inventoryv1.Storage{{Id: "local-zfs", Type: "zfspool", ZfsPool: "rpool"}},
		ZfsDatasets: []*inventoryv1.ZfsDataset{
			{Name: "rpool/subvol-101-disk-0", UsedBytes: 10 * gib},
			{Name: "rpool/vm-201-disk-0", UsedBytes: 30 * gib},
		},
	}
}

func drInv() *inventoryv1.Inventory {
	return &inventoryv1.Inventory{
		Host: &inventoryv1.HostInfo{Hostname: "pve2"},
		Storages: []*inventoryv1.Storage{
			{Id: "tank-dr", Type: "zfspool", ZfsPool: "tank-dr"},
			{Id: "local-lvm", Type: "lvmthin"},
		},
		ZfsPools: []*inventoryv1.ZfsPool{{Name: "tank-dr", FreeBytes: 100 * gib}},
		Interfaces: []*inventoryv1.NetworkInterface{
			{Name: "vmbr0", Type: "bridge", BridgePorts: []string{"nic0"}, VlanAware: true},
			{Name: "vmbr2", Type: "bridge", VlanAware: true},
			{Name: "vmbr99", Type: "bridge", VlanAware: true},
		},
	}
}

func ctxFor(primary, dr *inventoryv1.Inventory) Context {
	now := time.Now()
	return Context{
		Primary: &Host{ID: "p", Hostname: "pve1", Inventory: primary, ReceivedAt: now},
		DR:      &Host{ID: "d", Hostname: "pve2", Inventory: dr, ReceivedAt: now},
		Now:     now,
	}
}

func baseSpec(vmids ...uint32) *planv1.PlanSpec {
	s := &planv1.PlanSpec{Name: "Main", PrimaryHostId: "p", DrHostId: "d"}
	for _, id := range vmids {
		s.Guests = append(s.Guests, &planv1.PlanGuest{Vmid: id})
	}
	return s
}

func messages(is []*planv1.Issue, sev planv1.Severity) string {
	var out []string
	for _, i := range is {
		if i.Severity == sev {
			out = append(out, i.Message)
		}
	}
	return strings.Join(out, "\n")
}

func TestSuggest(t *testing.T) {
	s := Suggest(baseSpec(101, 201), primaryInv(), drInv())

	if s.IntervalSeconds != DefaultIntervalSeconds || s.SnapshotPrefix != DefaultSnapshotPrefix ||
		Grid(s.DrRetention) != "1x1d(keep=all) | 14x1d | 8x7d" || Grid(s.PrimaryRetention) != "1x1d(keep=all)" {
		t.Errorf("defaults = %d %q %q %q", s.IntervalSeconds, s.SnapshotPrefix, Grid(s.DrRetention), Grid(s.PrimaryRetention))
	}
	if len(s.StorageMappings) != 1 ||
		s.StorageMappings[0].ReceiveDataset != "tank-dr/ezdr/pve1" {
		t.Errorf("storage mappings = %v", s.StorageMappings)
	}
	// Two portless VLAN-aware bridges: ambiguous, so left for the user.
	if len(s.NetworkMappings) != 1 || s.NetworkMappings[0].TargetBridge != "" || s.TestBridge != "" {
		t.Errorf("network = %v, test bridge = %q", s.NetworkMappings, s.TestBridge)
	}
	// Once the user picks vmbr2, the remaining portless bridge becomes the
	// test bridge suggestion.
	s.NetworkMappings[0].TargetBridge = "vmbr2"
	if s = Suggest(s, primaryInv(), drInv()); s.TestBridge != "vmbr99" {
		t.Errorf("test bridge = %q, want vmbr99", s.TestBridge)
	}

	// Startup: 201 has order=2,up=30; 101 has none, so it goes after.
	got := map[uint32][2]uint32{}
	for _, g := range s.Guests {
		got[g.Vmid] = [2]uint32{uint32(g.StartupOrder), g.StartupDelaySeconds} //nolint:gosec // test values
	}
	if got[201] != [2]uint32{2, 30} || got[101] != [2]uint32{3, 0} {
		t.Errorf("startup = %v", got)
	}

	// A bridge with the same name on the DR host is suggested directly.
	dr := drInv()
	dr.Interfaces = append(dr.Interfaces, &inventoryv1.NetworkInterface{Name: "vmbr1", Type: "bridge", VlanAware: true})
	if s := Suggest(baseSpec(101), primaryInv(), dr); s.NetworkMappings[0].TargetBridge != "vmbr1" {
		t.Errorf("same-name bridge not suggested: %v", s.NetworkMappings)
	}
}

func validSpec() *planv1.PlanSpec {
	s := Suggest(baseSpec(101, 201), primaryInv(), drInv())
	s.GetNetwork().GetExisting().PrimaryAddress = "192.0.2.10"
	s.NetworkMappings[0].TargetBridge = "vmbr2"
	s.TestBridge = "vmbr99"
	return s
}

func TestValidateValidPlan(t *testing.T) {
	is := Validate(validSpec(), ctxFor(primaryInv(), drInv()))
	if errs := messages(is, planv1.Severity_SEVERITY_ERROR); errs != "" {
		t.Fatalf("unexpected errors:\n%s", errs)
	}
	warns := messages(is, planv1.Severity_SEVERITY_WARNING)
	for _, want := range []string{"PCI passthrough", "unconfigured guests: 202 (legacy)"} {
		if !strings.Contains(warns, want) {
			t.Errorf("missing warning %q in:\n%s", want, warns)
		}
	}
	if strings.Contains(warns, "300") {
		t.Error("templates should not be reported as unprotected")
	}
}

func TestValidateErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*planv1.PlanSpec, *Context)
		want   string
	}{
		"no name":           {func(s *planv1.PlanSpec, _ *Context) { s.Name = " " }, "needs a name"},
		"same hosts":        {func(s *planv1.PlanSpec, _ *Context) { s.DrHostId = "p" }, "must be different"},
		"missing inventory": {func(_ *planv1.PlanSpec, c *Context) { c.DR.Inventory = nil }, "hasn't reported"},
		"no guests":         {func(s *planv1.PlanSpec, _ *Context) { s.Guests = nil }, "at least one guest"},
		"guest gone":        {func(s *planv1.PlanSpec, _ *Context) { s.Guests[0].Vmid = 999 }, "no longer exists"},
		"not replicable":    {func(s *planv1.PlanSpec, _ *Context) { s.Guests[0].Vmid = 202 }, "can't be replicated"},
		"other plan":        {func(_ *planv1.PlanSpec, c *Context) { c.OtherPlans = map[uint32]string{101: "Other"} }, `protected by plan "Other"`},
		"vmid used on DR": {func(_ *planv1.PlanSpec, c *Context) {
			c.DR.Inventory.Guests = []*inventoryv1.Guest{{Vmid: 101, Name: "x"}}
		}, "already used on pve2"},
		"unmapped storage":   {func(s *planv1.PlanSpec, _ *Context) { s.StorageMappings = nil }, "choose where its replicas are stored"},
		"no dataset":         {func(s *planv1.PlanSpec, _ *Context) { s.StorageMappings[0].ReceiveDataset = "" }, "choose where its replicas are stored"},
		"bad dataset":        {func(s *planv1.PlanSpec, _ *Context) { s.StorageMappings[0].ReceiveDataset = "tank dr/x" }, "isn't a valid ZFS dataset"},
		"bad receive pool":   {func(s *planv1.PlanSpec, _ *Context) { s.StorageMappings[0].ReceiveDataset = "rpool/x" }, `pool "rpool" doesn't exist`},
		"unmapped bridge":    {func(s *planv1.PlanSpec, _ *Context) { s.NetworkMappings = nil }, "isn't mapped to a DR bridge"},
		"missing bridge":     {func(s *planv1.PlanSpec, _ *Context) { s.NetworkMappings[0].TargetBridge = "vmbr7" }, "doesn't exist on the DR host"},
		"missing test":       {func(s *planv1.PlanSpec, _ *Context) { s.TestBridge = "vmbr7" }, "test failover bridge"},
		"interval too short": {func(s *planv1.PlanSpec, _ *Context) { s.IntervalSeconds = 30 }, "between 1 minute and 24 hours"},
		"bad prefix":         {func(s *planv1.PlanSpec, _ *Context) { s.SnapshotPrefix = "has space" }, "snapshot prefix"},
		"empty retention":    {func(s *planv1.PlanSpec, _ *Context) { s.DrRetention = nil }, "DR host retention needs"},
		"primary too short": {func(s *planv1.PlanSpec, _ *Context) {
			s.IntervalSeconds = 3600
			s.PrimaryRetention = []*planv1.RetentionTier{{Count: 1, PeriodSeconds: 60, KeepAll: true}}
		}, "at least one snapshot interval"},
		"bad A record": {func(s *planv1.PlanSpec, _ *Context) {
			s.Guests[0].DnsRecords = []*planv1.DnsRecord{{Name: "app.example.com", Type: planv1.DnsRecordType_DNS_RECORD_TYPE_A,
				ProductionValue: "203.0.113.10", FailoverValue: "not-an-ip"}}
		}, "must be an IPv4 address"},
		"bad DNS name": {func(s *planv1.PlanSpec, _ *Context) {
			s.Guests[0].DnsRecords = []*planv1.DnsRecord{{Name: "bad name", Type: planv1.DnsRecordType_DNS_RECORD_TYPE_A,
				ProductionValue: "203.0.113.10", FailoverValue: "198.51.100.10"}}
		}, "not a valid DNS name"},
	} {
		t.Run(name, func(t *testing.T) {
			s, c := validSpec(), ctxFor(primaryInv(), drInv())
			tc.change(s, &c)
			if errs := messages(Validate(s, c), planv1.Severity_SEVERITY_ERROR); !strings.Contains(errs, tc.want) {
				t.Errorf("want error containing %q, got:\n%s", tc.want, errs)
			}
		})
	}
}

func TestValidateWarnings(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*planv1.PlanSpec, *Context)
		want   string
	}{
		"low space":      {func(_ *planv1.PlanSpec, c *Context) { c.DR.Inventory.ZfsPools[0].FreeBytes = 40 * gib }, `DR pool "tank-dr" has 40.0 GiB free`},
		"not VLAN-aware": {func(_ *planv1.PlanSpec, c *Context) { c.DR.Inventory.Interfaces[1].VlanAware = false }, "isn't VLAN-aware"},
		"no test bridge": {func(s *planv1.PlanSpec, _ *Context) { s.TestBridge = "" }, "no test failover bridge"},
		"test has ports": {func(s *planv1.PlanSpec, _ *Context) { s.TestBridge = "vmbr0" }, "has physical ports"},
		"stale":          {func(_ *planv1.PlanSpec, c *Context) { c.Primary.ReceivedAt = c.Now.Add(-2 * time.Hour) }, "2h0m0s old"},
		"unused mapping": {func(s *planv1.PlanSpec, _ *Context) {
			s.NetworkMappings = append(s.NetworkMappings, &planv1.NetworkMapping{SourceBridge: "vmbr5", TargetBridge: "vmbr2"})
		}, "isn't used by any protected guest"},
		"short RPO threshold": {func(s *planv1.PlanSpec, _ *Context) { s.RpoAlertSeconds = 2 * s.IntervalSeconds }, "alerts would fire during normal operation"},
		"same DNS values": {func(s *planv1.PlanSpec, _ *Context) {
			s.Guests[0].DnsRecords = []*planv1.DnsRecord{{Name: "app.example.com", Type: planv1.DnsRecordType_DNS_RECORD_TYPE_CNAME,
				ProductionValue: "x.example.com", FailoverValue: "x.example.com"}}
		}, "same production and failover value"},
	} {
		t.Run(name, func(t *testing.T) {
			s, c := validSpec(), ctxFor(primaryInv(), drInv())
			tc.change(s, &c)
			is := Validate(s, c)
			if errs := messages(is, planv1.Severity_SEVERITY_ERROR); errs != "" {
				t.Errorf("unexpected errors:\n%s", errs)
			}
			if w := messages(is, planv1.Severity_SEVERITY_WARNING); !strings.Contains(w, tc.want) {
				t.Errorf("want warning containing %q, got:\n%s", tc.want, w)
			}
		})
	}
}

func TestGrid(t *testing.T) {
	if g := Grid(RetentionPresets["hourly-daily"]); g != "1x1h(keep=all) | 24x1h | 14x1d" {
		t.Errorf("Grid = %q", g)
	}
	for sec, want := range map[uint32]string{300: "5m", 3600: "1h", 86400: "1d", 90: "90s", 604800: "7d"} {
		if got := Duration(sec); got != want {
			t.Errorf("Duration(%d) = %q, want %q", sec, got, want)
		}
	}
}

func TestNetworkValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*planv1.PlanSpec, *Context)
		want   string
	}{
		"no network":  {func(s *planv1.PlanSpec, _ *Context) { s.Network = nil }, "choose how the DR host reaches"},
		"no address":  {func(s *planv1.PlanSpec, _ *Context) { s.GetNetwork().GetExisting().PrimaryAddress = "" }, "primary's address"},
		"bad address": {func(s *planv1.PlanSpec, _ *Context) { s.GetNetwork().GetExisting().PrimaryAddress = "not an address!" }, "primary's address"},
		"bad listen": {func(s *planv1.PlanSpec, _ *Context) {
			s.GetNetwork().GetExisting().ListenAddress = "primary.example.com"
		}, "listen address must be an IP"},
		"test ID used": {func(_ *planv1.PlanSpec, c *Context) {
			c.DR.Inventory.Guests = []*inventoryv1.Guest{{Vmid: 10101, Name: "other"}}
		}, "test failover ID 10101 is already used"},
		"test ID protected": {func(s *planv1.PlanSpec, _ *Context) { s.TestVmidOffset = 100 }, "another protected guest's ID"},
		"test ID too large": {func(s *planv1.PlanSpec, _ *Context) { s.TestVmidOffset = 999999990 }, "too large"},
		"test time limit":   {func(s *planv1.PlanSpec, _ *Context) { s.TestTimeLimitSeconds = 60 }, "between 15 minutes and 7 days"},
		"low port":          {func(s *planv1.PlanSpec, _ *Context) { s.GetNetwork().GetExisting().Port = 80 }, "between 1024 and 65535"},
		"port in use":       {func(_ *planv1.PlanSpec, c *Context) { c.UsedPorts = map[uint32]string{8888: "Other"} }, `already used by plan "Other"`},
		"tunnel listener": {func(s *planv1.PlanSpec, _ *Context) {
			s.Network = &planv1.ReplicationNetwork{Path: &planv1.ReplicationNetwork_Tunnel{Tunnel: &planv1.EzdrTunnel{
				Endpoint: "dr.example.com:51821", ListenPort: 51821}}}
		}, "choose which host accepts"},
		"tunnel endpoint": {func(s *planv1.PlanSpec, _ *Context) {
			s.Network = &planv1.ReplicationNetwork{Path: &planv1.ReplicationNetwork_Tunnel{Tunnel: &planv1.EzdrTunnel{
				Listener: planv1.EzdrTunnel_LISTENER_DR, Endpoint: "dr.example.com", ListenPort: 51821}}}
		}, "public endpoint as host:port"},
		"tunnel conflict": {func(s *planv1.PlanSpec, c *Context) {
			s.Network = &planv1.ReplicationNetwork{Path: &planv1.ReplicationNetwork_Tunnel{Tunnel: &planv1.EzdrTunnel{
				Listener: planv1.EzdrTunnel_LISTENER_DR, Endpoint: "dr.example.com:51821", ListenPort: 51821}}}
			c.OtherTunnels = []OtherTunnel{{Plan: "Other", PrimaryHostID: "p", DRHostID: "d", Tunnel: &planv1.EzdrTunnel{
				Listener: planv1.EzdrTunnel_LISTENER_DR, Endpoint: "dr.example.com:51999", ListenPort: 51999}}}
		}, "share one tunnel"},
		"listen port clash": {func(s *planv1.PlanSpec, c *Context) {
			s.Network = &planv1.ReplicationNetwork{Path: &planv1.ReplicationNetwork_Tunnel{Tunnel: &planv1.EzdrTunnel{
				Listener: planv1.EzdrTunnel_LISTENER_DR, Endpoint: "dr.example.com:51821", ListenPort: 51821}}}
			c.OtherTunnels = []OtherTunnel{{Plan: "Other", PrimaryHostID: "x", DRHostID: "d", Tunnel: &planv1.EzdrTunnel{
				Listener: planv1.EzdrTunnel_LISTENER_DR, Endpoint: "dr.example.com:51999", ListenPort: 51999}}}
		}, "already accepts tunnels on port 51999"},
	} {
		t.Run(name, func(t *testing.T) {
			s, c := validSpec(), ctxFor(primaryInv(), drInv())
			tc.change(s, &c)
			if errs := messages(Validate(s, c), planv1.Severity_SEVERITY_ERROR); !strings.Contains(errs, tc.want) {
				t.Errorf("want error containing %q, got:\n%s", tc.want, errs)
			}
		})
	}
}

func TestListenAddressWarning(t *testing.T) {
	s, c := validSpec(), ctxFor(primaryInv(), drInv())
	s.GetNetwork().GetExisting().ListenAddress = "198.51.100.7"
	if w := messages(Validate(s, c), planv1.Severity_SEVERITY_WARNING); !strings.Contains(w, "isn't an address of any") {
		t.Errorf("want a warning about the listen address, got:\n%s", w)
	}
}

func TestJobGroups(t *testing.T) {
	inv := primaryInv()
	// Guest 201's disk is encrypted, so it gets its own job group and port.
	inv.ZfsDatasets[1].Encryption = "aes-256-gcm"
	s := validSpec()
	groups := JobGroups(s, inv)
	if len(groups) != 2 {
		t.Fatalf("groups = %+v", groups)
	}
	if groups[0].Encrypted || groups[0].Port != 8888 || groups[0].Datasets[0] != "rpool/subvol-101-disk-0" {
		t.Errorf("first group = %+v", groups[0])
	}
	if !groups[1].Encrypted || groups[1].Port != 8889 || groups[1].Datasets[0] != "rpool/vm-201-disk-0" ||
		groups[1].ReceiveDataset != "tank-dr/ezdr/pve1" {
		t.Errorf("second group = %+v", groups[1])
	}
	is := Validate(s, ctxFor(inv, drInv()))
	if w := messages(is, planv1.Severity_SEVERITY_WARNING); !strings.Contains(w, "2 zrepl ports on the primary (8888-8889)") {
		t.Errorf("missing multi-port warning:\n%s", w)
	}
}

func TestValidTunnel(t *testing.T) {
	s, c := validSpec(), ctxFor(primaryInv(), drInv())
	s.Network = &planv1.ReplicationNetwork{Path: &planv1.ReplicationNetwork_Tunnel{Tunnel: &planv1.EzdrTunnel{
		Listener: planv1.EzdrTunnel_LISTENER_DR, Endpoint: "203.0.113.20:51821", ListenPort: 51821, Port: 8888}}}
	// The same settings in another plan between the same hosts are fine.
	c.OtherTunnels = []OtherTunnel{{Plan: "Other", PrimaryHostID: "p", DRHostID: "d", Tunnel: s.GetNetwork().GetTunnel()}}
	if errs := messages(Validate(s, c), planv1.Severity_SEVERITY_ERROR); errs != "" {
		t.Errorf("unexpected errors:\n%s", errs)
	}
}

func TestRunningTestGuestsDontConflict(t *testing.T) {
	c := ctxFor(primaryInv(), drInv())
	c.DR.Inventory.Guests = []*inventoryv1.Guest{{Vmid: 10101, Name: "web", Tags: []string{"web", TestTag}}}
	if errs := messages(Validate(validSpec(), c), planv1.Severity_SEVERITY_ERROR); errs != "" {
		t.Errorf("a running test's guest counted as a conflict:\n%s", errs)
	}
}

func TestIssueSections(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*planv1.PlanSpec)
		want   string
	}{
		"name":        {func(s *planv1.PlanSpec) { s.Name = "" }, SectionGeneral},
		"no guests":   {func(s *planv1.PlanSpec) { s.Guests = nil }, SectionGuests},
		"guest gone":  {func(s *planv1.PlanSpec) { s.Guests[0].Vmid = 999 }, SectionGuests},
		"interval":    {func(s *planv1.PlanSpec) { s.IntervalSeconds = 1 }, SectionSchedule},
		"prefix":      {func(s *planv1.PlanSpec) { s.SnapshotPrefix = "bad prefix" }, SectionAdvanced},
		"test bridge": {func(s *planv1.PlanSpec) { s.TestBridge = "vmbr404" }, SectionMappings},
		"address":     {func(s *planv1.PlanSpec) { s.GetNetwork().GetExisting().PrimaryAddress = "" }, SectionNetwork},
		"timeout":     {func(s *planv1.PlanSpec) { s.ShutdownTimeoutSeconds = 1 }, SectionAdvanced},
		"dns": {func(s *planv1.PlanSpec) {
			s.Guests[0].DnsRecords = []*planv1.DnsRecord{{Name: "bad name", Type: planv1.DnsRecordType_DNS_RECORD_TYPE_A,
				ProductionValue: "192.0.2.1", FailoverValue: "198.51.100.1"}}
		}, SectionDNS},
	} {
		t.Run(name, func(t *testing.T) {
			s := validSpec()
			tc.change(s)
			var got []string
			for _, is := range Validate(s, ctxFor(primaryInv(), drInv())) {
				if is.Severity == planv1.Severity_SEVERITY_ERROR {
					got = append(got, is.Section)
				}
			}
			if len(got) == 0 || got[0] != tc.want {
				t.Errorf("error sections = %v, want %s first", got, tc.want)
			}
		})
	}
	// Warnings have sections too.
	for _, is := range Validate(validSpec(), ctxFor(primaryInv(), drInv())) {
		if is.Section == "" {
			t.Errorf("issue without a section: %s", is.Message)
		}
	}
}

func TestFailedOverGuestsOnDR(t *testing.T) {
	ctx := ctxFor(primaryInv(), drInv())
	ctx.DR.Inventory.Guests = []*inventoryv1.Guest{{Vmid: 101, Name: "web", Tags: []string{"ezdr-failover"}}}
	if errs := messages(Validate(validSpec(), ctx), planv1.Severity_SEVERITY_ERROR); !strings.Contains(errs, "already used") {
		t.Fatalf("an active plan's guest ID on the DR host isn't reported: %s", errs)
	}
	ctx.FailedOver = true
	if errs := messages(Validate(validSpec(), ctx), planv1.Severity_SEVERITY_ERROR); errs != "" {
		t.Errorf("a failed-over plan's own guests are reported as conflicts:\n%s", errs)
	}
}

func TestExcludedGuestsArentUnconfigured(t *testing.T) {
	ctx := ctxFor(primaryInv(), drInv())
	ctx.Excluded = map[uint32]bool{202: true}
	if warns := messages(Validate(validSpec(), ctx), planv1.Severity_SEVERITY_WARNING); strings.Contains(warns, "unconfigured") {
		t.Errorf("an excluded guest is reported as unconfigured:\n%s", warns)
	}
}

func TestSuggestDatasetFallsBackToFreestPool(t *testing.T) {
	dr := drInv()
	dr.Storages = nil
	dr.ZfsPools = []*inventoryv1.ZfsPool{{Name: "small", FreeBytes: 1 << 30}, {Name: "big", FreeBytes: 1 << 40}}
	s := Suggest(baseSpec(101), primaryInv(), dr)
	if len(s.StorageMappings) != 1 || !strings.HasPrefix(s.StorageMappings[0].ReceiveDataset, "big/ezdr/") {
		t.Errorf("mappings = %v", s.StorageMappings)
	}
}
