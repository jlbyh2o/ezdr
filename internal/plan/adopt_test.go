package plan

import (
	"strings"
	"testing"

	inventoryv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/inventory/v1"
	planv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/plan/v1"
)

// withHandWritten adds a hand-written setup shaped like a typical manual
// configuration: the whole pool except guest 201, into tank-dr/replicated.
func withHandWritten(primary, dr *inventoryv1.Inventory) {
	primary.ZfsDatasets = append(primary.ZfsDatasets, &inventoryv1.ZfsDataset{Name: "rpool"})
	primary.Zrepl = &inventoryv1.Zrepl{Version: "v0.6.1", Running: true, Jobs: []*inventoryv1.ZreplJob{{
		Name: "pve1_source", Type: "source", Transport: "tls", ListenAddress: "192.0.2.10:8888", ListenFreebind: true,
		Filesystems: []*inventoryv1.ZreplFilter{
			{Pattern: "rpool<", Include: true}, {Pattern: "rpool/vm-201-disk-0"},
		},
		SnapshottingType: "periodic", SnapshotPrefix: "zrepl_", SnapshotIntervalSeconds: 300,
		Send: &inventoryv1.ZreplSend{Compressed: true, LargeBlocks: true, EmbeddedData: true},
	}}}
	sender := []*inventoryv1.ZreplPruneRule{
		{Type: "not_replicated"},
		{Type: "grid", Grid: "1x1h(keep=all) | 24x1h | 7x1d", Regex: "^zrepl_"},
		{Type: "regex", Regex: "^zrepl_", Negate: true},
	}
	receiver := []*inventoryv1.ZreplPruneRule{
		{Type: "grid", Grid: "1x1h(keep=all) | 24x1h | 14x1d", Regex: "^zrepl_"},
		{Type: "regex", Regex: "^zrepl_", Negate: true},
	}
	dr.Zrepl = &inventoryv1.Zrepl{Version: "v0.6.1", Running: true, Jobs: []*inventoryv1.ZreplJob{
		{Name: "dr_pull", Type: "pull", Transport: "tls", ConnectAddress: "192.0.2.10:8888", RootFs: "tank-dr/replicated",
			IntervalSeconds: 300, KeepSender: sender, KeepReceiver: receiver},
		// EZDR's own jobs are never offered.
		{Name: "ezdr_x_pull", Type: "pull", Managed: true, ConnectAddress: "192.0.2.10:8888"},
		// Different port: not this source job's partner.
		{Name: "other_pull", Type: "pull", ConnectAddress: "192.0.2.10:9999"},
	}}
}

func TestFilterIncludes(t *testing.T) {
	f := []*inventoryv1.ZreplFilter{
		{Pattern: "rpool<", Include: true},
		{Pattern: "rpool/data<"},
		{Pattern: "rpool/data/keep", Include: true},
		{Pattern: "rpool/vm-1-disk-0"},
	}
	for ds, want := range map[string]bool{
		"rpool": true, "rpool/vm-2-disk-0": true, "rpool/vm-1-disk-0": false, "rpool/data": false,
		"rpool/data/x": false, "rpool/data/keep": true, "rpool/data/keep/child": false, "tank/x": false,
		"rpoolx": false,
	} {
		if got := FilterIncludes(f, ds); got != want {
			t.Errorf("FilterIncludes(%q) = %v, want %v", ds, got, want)
		}
	}
}

func TestZreplSetups(t *testing.T) {
	p, d := primaryInv(), drInv()
	withHandWritten(p, d)
	setups := ZreplSetups(p, d)
	if len(setups) != 1 || setups[0].Source.Name != "pve1_source" || setups[0].Pull.Name != "dr_pull" {
		t.Errorf("setups = %v", setups)
	}
}

func TestAdopt(t *testing.T) {
	p, d := primaryInv(), drInv()
	withHandWritten(p, d)
	// The plan protected 201 before; the old job doesn't replicate it.
	s, notes, err := Adopt(baseSpec(201), p, d, "pve1_source", "dr_pull")
	if err != nil {
		t.Fatal(err)
	}
	if s.Takeover.GetSourceJob() != "pve1_source" || s.Takeover.GetPullJob() != "dr_pull" {
		t.Errorf("takeover = %v", s.Takeover)
	}
	if s.SnapshotPrefix != "zrepl_" || s.IntervalSeconds != 300 ||
		Grid(s.PrimaryRetention) != "1x1h(keep=all) | 24x1h | 7x1d" || Grid(s.DrRetention) != "1x1h(keep=all) | 24x1h | 14x1d" {
		t.Errorf("settings = %q %d %q %q", s.SnapshotPrefix, s.IntervalSeconds, Grid(s.PrimaryRetention), Grid(s.DrRetention))
	}
	e := s.GetNetwork().GetExisting()
	if e.GetPrimaryAddress() != "192.0.2.10" || e.GetPort() != 8888 || e.GetListenAddress() != "192.0.2.10" {
		t.Errorf("network = %v", s.Network)
	}
	if len(s.Guests) != 1 || s.Guests[0].Vmid != 101 {
		t.Errorf("guests = %v", s.Guests)
	}
	if len(s.StorageMappings) != 1 || s.StorageMappings[0].ReceiveDataset != "tank-dr/replicated" {
		t.Errorf("storage = %v", s.StorageMappings)
	}
	if got := strings.Join(notes, "\n"); !strings.Contains(got, "guest 201 is no longer protected") {
		t.Errorf("notes = %s", got)
	}

	if _, _, err := Adopt(baseSpec(), p, d, "pve1_source", "other_pull"); err == nil {
		t.Error("adopting a pull job on another port succeeded")
	}
}

func TestAdoptNotes(t *testing.T) {
	p, d := primaryInv(), drInv()
	withHandWritten(p, d)
	// Only one of 201's disks was replicated, the pull job ran less often,
	// and the receiver kept the newest snapshots by count.
	p.Guests[1].Disks = append(p.Guests[1].Disks, replicable("scsi1", "local-zfs", "vm-201-disk-1", "rpool"))
	p.Zrepl.Jobs[0].Filesystems[1] = &inventoryv1.ZreplFilter{Pattern: "rpool/vm-201-disk-1"}
	pull := d.Zrepl.Jobs[0]
	pull.IntervalSeconds = 900
	pull.KeepReceiver = append(pull.KeepReceiver, &inventoryv1.ZreplPruneRule{Type: "last_n", Count: 5})
	pull.KeepSender = pull.KeepSender[:2]

	s, notes, err := Adopt(baseSpec(), p, d, "pve1_source", "dr_pull")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Guests) != 2 {
		t.Errorf("guests = %v", s.Guests)
	}
	got := strings.Join(notes, "\n")
	for _, want := range []string{
		"guest 201 (app): the old job skipped rpool/vm-201-disk-1",
		"old pull job ran every 15m",
		"the last_n 5 rule isn't carried over",
		"primary retention: EZDR keeps snapshots without the prefix",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing note %q in:\n%s", want, got)
		}
	}
}

func TestValidateTakeover(t *testing.T) {
	p, d := primaryInv(), drInv()
	withHandWritten(p, d)
	adopted, _, err := Adopt(validSpec(), p, d, "pve1_source", "dr_pull")
	if err != nil {
		t.Fatal(err)
	}
	// Protect 201 too, which the old job skipped.
	adopted.Guests = append(adopted.Guests, &planv1.PlanGuest{Vmid: 201, StartupOrder: 9})
	is := Validate(adopted, ctxFor(p, d))
	if errs := messages(is, planv1.Severity_SEVERITY_ERROR); errs != "" {
		t.Fatalf("unexpected errors:\n%s", errs)
	}
	warns := messages(is, planv1.Severity_SEVERITY_WARNING)
	for _, want := range []string{
		"doesn't replicate rpool/vm-201-disk-0, so it will need a full send",
		"also replicates rpool; after the takeover it is no longer replicated",
	} {
		if !strings.Contains(warns, want) {
			t.Errorf("missing warning %q in:\n%s", want, warns)
		}
	}

	for name, tc := range map[string]struct {
		change func(*planv1.PlanSpec)
		want   string
	}{
		"prefix":  {func(s *planv1.PlanSpec) { s.SnapshotPrefix = "ezdr_" }, `use the prefix "zrepl_"`},
		"receive": {func(s *planv1.PlanSpec) { s.StorageMappings[0].ReceiveDataset = "tank-dr/other" }, "receives into tank-dr/replicated"},
		"gone":    {func(s *planv1.PlanSpec) { s.Takeover.PullJob = "missing" }, "weren't found"},
	} {
		t.Run(name, func(t *testing.T) {
			s, _, _ := Adopt(validSpec(), p, d, "pve1_source", "dr_pull")
			tc.change(s)
			if errs := messages(Validate(s, ctxFor(p, d)), planv1.Severity_SEVERITY_ERROR); !strings.Contains(errs, tc.want) {
				t.Errorf("want error containing %q, got:\n%s", tc.want, errs)
			}
		})
	}
}
