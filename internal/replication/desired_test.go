package replication

import (
	"bytes"
	"net/netip"
	"strings"
	"testing"

	inventoryv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/inventory/v1"
	planv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/plan/v1"
)

func fixture() (Plan, map[string]*Host) {
	primary := &Host{ID: "p1", Hostname: "pve1", Certificate: "P-CERT", Inventory: &inventoryv1.Inventory{
		Guests: []*inventoryv1.Guest{
			{Vmid: 101, Disks: []*inventoryv1.Disk{{Key: "rootfs", Storage: "local-zfs", ZfsDataset: "rpool/subvol-101-disk-0",
				Readiness: inventoryv1.Readiness_READINESS_REPLICABLE}}},
			{Vmid: 102, Disks: []*inventoryv1.Disk{{Key: "rootfs", Storage: "local-zfs", ZfsDataset: "rpool/subvol-102-disk-0",
				Readiness: inventoryv1.Readiness_READINESS_REPLICABLE}}},
		},
		ZfsDatasets: []*inventoryv1.ZfsDataset{{Name: "rpool/subvol-101-disk-0", Encryption: "off"}, {Name: "rpool/subvol-102-disk-0", Encryption: "off"}},
	}}
	dr := &Host{ID: "d1", Hostname: "dr1", Certificate: "D-CERT", Inventory: &inventoryv1.Inventory{}}
	p := Plan{ID: "abcdefghijk", Name: "Main", Spec: &planv1.PlanSpec{
		PrimaryHostId: "p1", DrHostId: "d1", IntervalSeconds: 300, SnapshotPrefix: "zrepl_",
		Guests:          []*planv1.PlanGuest{{Vmid: 101}, {Vmid: 102}},
		StorageMappings: []*planv1.StorageMapping{{SourceStorage: "local-zfs", TargetStorage: "tank", ReceiveDataset: "tank/replicated"}},
		Network: &planv1.ReplicationNetwork{Path: &planv1.ReplicationNetwork_Existing{
			Existing: &planv1.ExistingNetwork{PrimaryAddress: "192.0.2.12", Port: 8888}}},
		PrimaryRetention: []*planv1.RetentionTier{{Count: 1, PeriodSeconds: 3600, KeepAll: true}},
		DrRetention:      []*planv1.RetentionTier{{Count: 1, PeriodSeconds: 3600, KeepAll: true}, {Count: 14, PeriodSeconds: 86400}},
	}}
	return p, map[string]*Host{"p1": primary, "d1": dr}
}

func TestDesired(t *testing.T) {
	p, hosts := fixture()

	src, probs := Desired("p1", []Plan{p}, hosts)
	if len(probs) != 0 || len(src.SourceJobs) != 1 || len(src.PullJobs) != 0 {
		t.Fatalf("primary desired = %v, problems %v", src, probs)
	}
	j := src.SourceJobs[0]
	if j.Name != "ezdr_abcdefgh_local-zfs" || j.ListenAddress != ":8888" || len(j.Datasets) != 2 ||
		j.Peer.Name != "ezdr-d1" || j.Peer.CertificatePem != "D-CERT" || j.SnapshotPrefix != "zrepl_" {
		t.Errorf("source job = %v", j)
	}

	pull, _ := Desired("d1", []Plan{p}, hosts)
	if len(pull.PullJobs) != 1 || len(pull.SourceJobs) != 0 {
		t.Fatalf("DR desired = %v", pull)
	}
	pj := pull.PullJobs[0]
	if pj.Name != "ezdr_abcdefgh_local-zfs_pull" || pj.Address != "192.0.2.12:8888" || pj.ReceiveDataset != "tank/replicated" ||
		pj.Peer.Name != "ezdr-p1" || pj.Peer.CertificatePem != "P-CERT" || len(pj.DrRetention) != 2 {
		t.Errorf("pull job = %v", pj)
	}

	// A specific listen address is bound with freebind.
	p.Spec.GetNetwork().GetExisting().ListenAddress = "192.0.2.12"
	src, _ = Desired("p1", []Plan{p}, hosts)
	if j := src.SourceJobs[0]; j.ListenAddress != "192.0.2.12:8888" || !j.ListenFreebind {
		t.Errorf("source job with listen address = %v", j)
	}

	// Unrelated hosts get nothing.
	if other, _ := Desired("x", []Plan{p}, hosts); len(other.SourceJobs)+len(other.PullJobs) != 0 {
		t.Errorf("unrelated host got jobs: %v", other)
	}
}

func TestDesiredWaitsForCertificates(t *testing.T) {
	p, hosts := fixture()
	hosts["d1"].Certificate = ""
	z, probs := Desired("p1", []Plan{p}, hosts)
	if len(z.SourceJobs) != 0 || len(probs) != 1 || !strings.Contains(probs[0], "waiting for dr1's zrepl certificate") {
		t.Errorf("desired = %v, problems = %v", z, probs)
	}
}

func TestChanges(t *testing.T) {
	p, hosts := fixture()
	before, _ := Desired("d1", nil, hosts)
	after, _ := Desired("d1", []Plan{p}, hosts)
	add := Changes(before, after)
	if len(add) != 1 || !strings.HasPrefix(add[0], "add zrepl job ezdr_abcdefgh_local-zfs_pull: pull from 192.0.2.12:8888 every 5m") {
		t.Errorf("add = %v", add)
	}
	p.Spec.IntervalSeconds = 900
	changed, _ := Desired("d1", []Plan{p}, hosts)
	if c := Changes(after, changed); len(c) != 1 || !strings.Contains(c[0], "change zrepl job") || !strings.Contains(c[0], "every 15m") {
		t.Errorf("change = %v", c)
	}
	if r := Changes(after, before); len(r) != 1 || !strings.Contains(r[0], "remove zrepl job") {
		t.Errorf("remove = %v", r)
	}
	if n := Changes(after, after); len(n) != 0 {
		t.Errorf("no-op = %v", n)
	}
}

func tunnelFixture() (Plan, map[string]*Host) {
	p, hosts := fixture()
	p.Spec.Network = &planv1.ReplicationNetwork{Path: &planv1.ReplicationNetwork_Tunnel{Tunnel: &planv1.EzdrTunnel{
		Listener: planv1.EzdrTunnel_LISTENER_DR, Endpoint: "dr.example.com:51821", ListenPort: 51821, Port: 8888,
	}}}
	hosts["p1"].SiteAddress, hosts["p1"].SitePublicKey = netip.MustParseAddr("100.64.43.1"), bytes.Repeat([]byte{1}, 32)
	hosts["d1"].SiteAddress, hosts["d1"].SitePublicKey = netip.MustParseAddr("100.64.43.2"), bytes.Repeat([]byte{2}, 32)
	return p, hosts
}

func TestTunnelJobs(t *testing.T) {
	p, hosts := tunnelFixture()
	src, _ := Desired("p1", []Plan{p}, hosts)
	if j := src.SourceJobs[0]; j.ListenAddress != "100.64.43.1:8888" || !j.ListenFreebind {
		t.Errorf("source job = %v", j)
	}
	pull, _ := Desired("d1", []Plan{p}, hosts)
	if j := pull.PullJobs[0]; j.Address != "100.64.43.1:8888" {
		t.Errorf("pull job = %v", j)
	}
}

func TestSiteTunnel(t *testing.T) {
	p, hosts := tunnelFixture()
	prefix := netip.MustParsePrefix("100.64.43.0/28")

	// The DR host listens; the primary connects to its endpoint with keepalives.
	dr, probs := SiteTunnel("d1", []Plan{p}, hosts, prefix)
	if len(probs) != 0 || dr.Address != "100.64.43.2" || dr.ListenPort != 51821 || len(dr.Peers) != 1 ||
		dr.Peers[0].Address != "100.64.43.1" || dr.Peers[0].Endpoint != "" || dr.Peers[0].PersistentKeepaliveSeconds != 0 {
		t.Errorf("DR tunnel = %v, %v", dr, probs)
	}
	pri, _ := SiteTunnel("p1", []Plan{p}, hosts, prefix)
	if pri.ListenPort != 0 || pri.Peers[0].Endpoint != "dr.example.com:51821" || pri.Peers[0].PersistentKeepaliveSeconds != TunnelKeepalive ||
		!bytes.Equal(pri.Peers[0].PublicKey, hosts["d1"].SitePublicKey) {
		t.Errorf("primary tunnel = %v", pri)
	}

	// Listener on the primary flips the roles.
	p.Spec.GetNetwork().GetTunnel().Listener = planv1.EzdrTunnel_LISTENER_PRIMARY
	p.Spec.GetNetwork().GetTunnel().Endpoint = "primary.example.com:51821"
	pri, _ = SiteTunnel("p1", []Plan{p}, hosts, prefix)
	dr, _ = SiteTunnel("d1", []Plan{p}, hosts, prefix)
	if pri.ListenPort != 51821 || dr.ListenPort != 0 || dr.Peers[0].Endpoint != "primary.example.com:51821" {
		t.Errorf("flipped: primary %v, DR %v", pri, dr)
	}

	// Missing key: no peer, and the jobs wait too.
	hosts["d1"].SitePublicKey = nil
	pri, probs = SiteTunnel("p1", []Plan{p}, hosts, prefix)
	if len(pri.GetPeers()) != 0 || len(probs) != 1 {
		t.Errorf("missing key: %v, %v", pri, probs)
	}
	if z, probs := Desired("p1", []Plan{p}, hosts); len(z.SourceJobs) != 0 || len(probs) == 0 {
		t.Errorf("jobs built without a tunnel key: %v", z)
	}

	// Existing-network plans don't create a tunnel.
	plain, hosts2 := fixture()
	if st, _ := SiteTunnel("p1", []Plan{plain}, hosts2, prefix); st != nil {
		t.Errorf("tunnel for an existing-network plan: %v", st)
	}
}
