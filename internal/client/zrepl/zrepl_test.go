package zrepl

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
)

func testPaths(t *testing.T) Paths {
	dir := t.TempDir()
	return Paths{
		MainConfig: filepath.Join(dir, "zrepl.yml"),
		JobsDir:    filepath.Join(dir, "ezdr.d"),
		CertDir:    filepath.Join(dir, "cert"),
		PeersDir:   filepath.Join(dir, "cert", "peers"),
	}
}

func peerCert(t *testing.T, name string) string {
	t.Helper()
	pemText, err := EnsureCertificate(testPaths(t), name)
	if err != nil {
		t.Fatal(err)
	}
	return pemText
}

func sampleZrepl(t *testing.T) *clientv1.Zrepl {
	return &clientv1.Zrepl{
		SourceJobs: []*clientv1.SourceJob{{
			Name: "ezdr_abcd1234_local-zfs", Datasets: []string{"rpool/data/vm-201-disk-0", "rpool/data/subvol-101-disk-0"},
			SnapshotPrefix: "zrepl_", IntervalSeconds: 300, ListenAddress: ":8888",
			Peer: &clientv1.Peer{Name: "ezdr-dr1", CertificatePem: peerCert(t, "ezdr-dr1")},
		}},
		PullJobs: []*clientv1.PullJob{{
			Name: "ezdr_abcd1234_local-zfs_pull", Address: "192.0.2.12:8888", ReceiveDataset: "tank/replicated",
			IntervalSeconds: 300, SnapshotPrefix: "zrepl_",
			Peer:             &clientv1.Peer{Name: "ezdr-p1", CertificatePem: peerCert(t, "ezdr-p1")},
			PrimaryRetention: []*clientv1.RetentionTier{{Count: 1, PeriodSeconds: 3600, KeepAll: true}},
			DrRetention:      []*clientv1.RetentionTier{{Count: 1, PeriodSeconds: 3600, KeepAll: true}, {Count: 24, PeriodSeconds: 3600}, {Count: 14, PeriodSeconds: 86400}},
		}},
	}
}

func TestCertificate(t *testing.T) {
	p := testPaths(t)
	a, err := EnsureCertificate(p, "ezdr-host1")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := EnsureCertificate(p, "ezdr-host1")
	if a != b {
		t.Error("certificate regenerated instead of reused")
	}
	if info, _ := os.Stat(p.KeyFile()); info.Mode().Perm() != 0o600 {
		t.Errorf("key permissions = %v", info.Mode().Perm())
	}
	if err := checkCertificate(a, "ezdr-host1"); err != nil {
		t.Error(err)
	}
	if err := checkCertificate(a, "ezdr-other"); err == nil {
		t.Error("certificate accepted for the wrong name")
	}
	if err := checkCertificate(a+a, "ezdr-host1"); err == nil {
		t.Error("two certificates accepted")
	}
}

func TestValidateRejects(t *testing.T) {
	for name, change := range map[string]func(*clientv1.Zrepl){
		"job name":      func(z *clientv1.Zrepl) { z.SourceJobs[0].Name = "x; rm -rf /" },
		"duplicate":     func(z *clientv1.Zrepl) { z.PullJobs[0].Name = z.SourceJobs[0].Name },
		"dataset":       func(z *clientv1.Zrepl) { z.SourceJobs[0].Datasets = []string{"rpool/../etc"} },
		"pool root":     func(z *clientv1.Zrepl) { z.SourceJobs[0].Datasets = []string{"rpool"} },
		"prefix":        func(z *clientv1.Zrepl) { z.SourceJobs[0].SnapshotPrefix = "a b" },
		"listen":        func(z *clientv1.Zrepl) { z.SourceJobs[0].ListenAddress = "8888" },
		"address":       func(z *clientv1.Zrepl) { z.PullJobs[0].Address = "bad host!:8888" },
		"port":          func(z *clientv1.Zrepl) { z.PullJobs[0].Address = "192.0.2.12:99999" },
		"root":          func(z *clientv1.Zrepl) { z.PullJobs[0].ReceiveDataset = "/tank" },
		"root dotdot":   func(z *clientv1.Zrepl) { z.PullJobs[0].ReceiveDataset = "tank/../x" },
		"interval":      func(z *clientv1.Zrepl) { z.PullJobs[0].IntervalSeconds = 5 },
		"retention":     func(z *clientv1.Zrepl) { z.PullJobs[0].DrRetention = nil },
		"peer name":     func(z *clientv1.Zrepl) { z.PullJobs[0].Peer.Name = "../../etc/x" },
		"peer cert":     func(z *clientv1.Zrepl) { z.PullJobs[0].Peer.CertificatePem = "not a cert" },
		"cert for peer": func(z *clientv1.Zrepl) { z.PullJobs[0].Peer.CertificatePem = z.SourceJobs[0].Peer.CertificatePem },
	} {
		z := sampleZrepl(t)
		change(z)
		if err := Validate(z); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := Validate(sampleZrepl(t)); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
}

func TestRender(t *testing.T) {
	p := testPaths(t)
	out, err := Render(sampleZrepl(t), p)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Jobs []map[string]any `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("rendered YAML doesn't parse: %v\n%s", err, out)
	}
	src, pull := doc.Jobs[0], doc.Jobs[1]
	if src["type"] != "source" || pull["type"] != "pull" {
		t.Fatalf("jobs = %v", doc.Jobs)
	}
	serve := src["serve"].(map[string]any)
	if serve["type"] != "tls" || serve["ca"] != p.PeerFile("ezdr-dr1") || serve["client_cns"].([]any)[0] != "ezdr-dr1" {
		t.Errorf("serve = %v", serve)
	}
	if src["snapshotting"].(map[string]any)["interval"] != "5m" {
		t.Errorf("snapshotting = %v", src["snapshotting"])
	}
	recv := pull["recv"].(map[string]any)
	if recv["placeholder"].(map[string]any)["encryption"] != "inherit" ||
		recv["properties"].(map[string]any)["override"].(map[string]any)["readonly"] != "on" {
		t.Errorf("recv = %v", recv)
	}
	pruning := pull["pruning"].(map[string]any)
	sender := pruning["keep_sender"].([]any)
	if sender[0].(map[string]any)["type"] != "not_replicated" {
		t.Error("sender pruning must keep unreplicated snapshots first")
	}
	receiver := pruning["keep_receiver"].([]any)
	if g := receiver[0].(map[string]any); g["grid"] != "1x1h(keep=all) | 24x1h | 14x1d" || g["regex"] != "^zrepl_" {
		t.Errorf("receiver grid = %v", g)
	}
	if n := receiver[1].(map[string]any); n["negate"] != true || n["regex"] != "^zrepl_" {
		t.Errorf("foreign snapshots must be kept: %v", n)
	}
	if !strings.HasPrefix(string(out), "# Managed by EZDR") {
		t.Error("missing managed-by header")
	}
}

func TestEnsureInclude(t *testing.T) {
	p := testPaths(t)
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

	// Missing: created with an absolute include.
	if _, changed, err := ensureInclude(p, now); err != nil || !changed {
		t.Fatalf("create: %v %v", changed, err)
	}
	b, _ := os.ReadFile(p.MainConfig)
	if !strings.Contains(string(b), "- "+p.JobsDir+"/") {
		t.Errorf("created config lacks include:\n%s", b)
	}

	// Existing hand-written config: jobs and comments survive.
	existing := "# my comment\nglobal:\n  logging:\n  - type: syslog\njobs:\n- name: mine\n  type: snap\n"
	if err := os.WriteFile(p.MainConfig, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	backup, changed, err := ensureInclude(p, now)
	if err != nil || !changed || backup == "" {
		t.Fatalf("edit: %q %v %v", backup, changed, err)
	}
	b, _ = os.ReadFile(p.MainConfig)
	for _, want := range []string{"# my comment", "name: mine", p.JobsDir + "/"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("edited config lacks %q:\n%s", want, b)
		}
	}
	if orig, _ := os.ReadFile(backup); string(orig) != existing { //nolint:gosec // test file
		t.Error("backup doesn't match the original")
	}
	if _, changed, _ := ensureInclude(p, now); changed {
		t.Error("second call changed the config again")
	}
}

type fakeRunner struct {
	// out maps exact command lines to their output.
	out        map[string]string
	calls      []string
	failCheck  bool
	notRunning bool
	datasets   map[string]bool
}

func (f *fakeRunner) run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, call)
	if o, ok := f.out[call]; ok {
		return []byte(o), nil
	}
	switch {
	case call == "zrepl version --show client":
		return []byte("client: zrepl version=v0.7.0 go=go1.25"), nil
	case call == "zrepl configcheck" && f.failCheck:
		return []byte("bad"), errors.New("exit status 1")
	case strings.HasPrefix(call, "systemctl is-active") && f.notRunning:
		return nil, errors.New("inactive")
	case strings.HasPrefix(call, "zfs list -Hp -t filesystem,volume,snapshot,bookmark "):
		return nil, errors.New("exit status 1: cannot open '" + args[len(args)-1] + "': dataset does not exist")
	case strings.HasPrefix(call, "zfs list -H -o name "):
		if !f.datasets[args[len(args)-1]] {
			return nil, errors.New("dataset does not exist")
		}
	case strings.HasPrefix(call, "zfs create -p -o canmount=off "):
		if f.datasets == nil {
			f.datasets = map[string]bool{}
		}
		f.datasets[args[len(args)-1]] = true
	}
	return nil, nil
}

func (f *fakeRunner) did(prefix string) bool {
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func TestApply(t *testing.T) {
	p := testPaths(t)
	if _, err := EnsureCertificate(p, "ezdr-self"); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{}
	a := &Applier{Paths: p, Run: f.run, Now: time.Now}
	ctx := context.Background()

	// Nothing to do without jobs or a jobs file.
	if err := a.Apply(ctx, &clientv1.Zrepl{}); err != nil || len(f.calls) != 0 {
		t.Fatalf("empty apply: %v, calls %v", err, f.calls)
	}

	z := sampleZrepl(t)
	if err := a.Apply(ctx, z); err != nil {
		t.Fatal(err)
	}
	if !f.did("zrepl configcheck") || !f.did("systemctl restart zrepl") {
		t.Errorf("first apply calls = %v", f.calls)
	}
	if !f.did("zfs create -p -o canmount=off tank/replicated") {
		t.Errorf("receive dataset not created: %v", f.calls)
	}
	first, _ := os.ReadFile(p.JobsFile())

	// Unchanged: no restart.
	f.calls = nil
	if err := a.Apply(ctx, z); err != nil {
		t.Fatal(err)
	}
	if f.did("systemctl restart") || f.did("zrepl configcheck") || f.did("zfs create") {
		t.Errorf("unchanged apply did unnecessary work: %v", f.calls)
	}

	// Rejected by configcheck: the previous file stays.
	f.calls, f.failCheck = nil, true
	z.SourceJobs[0].IntervalSeconds = 900
	if err := a.Apply(ctx, z); err == nil || !strings.Contains(err.Error(), "previous configuration kept") {
		t.Fatalf("rejected apply: %v", err)
	}
	if now, _ := os.ReadFile(p.JobsFile()); string(now) != string(first) {
		t.Error("rejected configuration wasn't rolled back")
	}
	if f.did("systemctl restart") {
		t.Error("zrepl restarted after a rejected configuration")
	}

	// No jobs any more: EZDR's jobs are removed.
	f.calls, f.failCheck = nil, false
	if err := a.Apply(ctx, &clientv1.Zrepl{}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p.JobsFile()); strings.Contains(string(b), "name:") {
		t.Errorf("jobs not removed:\n%s", b)
	}
	if !f.did("systemctl restart zrepl") {
		t.Error("zrepl not restarted after removing jobs")
	}
}

const sampleStatus = `{"Jobs":{"ezdr_abcd1234_local-zfs_pull":{"type":"pull","pull":{
"Replication":{"WaitReconnectError":null,"Attempts":[{"State":"done","StartAt":"2026-09-27T15:35:35-06:00",
"FinishAt":"2026-09-27T15:35:37-06:00","PlanError":null,"Filesystems":[
{"Info":{"Name":"rpool/data/vm-201-disk-0"},"State":"done","PlanError":null,"StepError":null,
 "Steps":[{"Info":{"From":"@zrepl_1","BytesExpected":60,"BytesReplicated":60}},{"Info":{"From":"@zrepl_1","BytesExpected":40,"BytesReplicated":30}}]},
{"Info":{"Name":"rpool/data/subvol-101-disk-0"},"State":"stepping","PlanError":null,"StepError":{"Err":"receive failed"},"Steps":[]}]}]},
"PruningSender":{"Error":"","Completed":[]},
"PruningReceiver":{"Error":"","Completed":[{"Filesystem":"rpool/data/vm-201-disk-0","LastError":"busy"}]}}},
"other_job":{"type":"push"}}}`

func TestStatus(t *testing.T) {
	f := &fakeRunner{}
	a := &Applier{Paths: testPaths(t), Now: time.Now, Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
		call := strings.Join(append([]string{name}, args...), " ")
		switch {
		case call == "zrepl status --mode raw":
			return []byte(sampleStatus), nil
		case strings.HasPrefix(call, "zfs list -H -p -t snapshot"):
			return []byte("tank/replicated/rpool/data/vm-201-disk-0@zrepl_1\t1000\n" +
				"tank/replicated/rpool/data/vm-201-disk-0@zrepl_2\t2000\n" +
				"tank/replicated/rpool/data/vm-201-disk-0@manual\t3000\n" +
				"tank/replicated/rpool/data/subvol-101-disk-0@zrepl_1\t1500\n"), nil
		}
		return f.run(ctx, name, args...)
	}}
	st := a.Status(context.Background(), sampleZrepl(t))
	if st.Error != "" || len(st.Jobs) != 1 {
		t.Fatalf("status = %v", st)
	}
	j := st.Jobs[0]
	if j.State != "done" || j.AttemptFinishedAt == nil || len(j.Errors) != 1 || !strings.Contains(j.Errors[0], "busy") {
		t.Errorf("job = %v", j)
	}
	byName := map[string]*clientv1.DatasetStatus{}
	for _, d := range j.Datasets {
		byName[d.Dataset] = d
	}
	vm := byName["rpool/data/vm-201-disk-0"]
	if vm == nil || vm.LatestSnapshot != "zrepl_2" || vm.LatestSnapshotAt.AsTime().Unix() != 2000 || vm.BytesReplicated != 90 || vm.FullSend {
		t.Errorf("vm dataset = %v (the manual snapshot must be ignored)", vm)
	}
	if ct := byName["rpool/data/subvol-101-disk-0"]; ct == nil || ct.Error != "receive failed" || ct.State != "stepping" {
		t.Errorf("ct dataset = %v", ct)
	}

	// No pull jobs: nothing to report, and zrepl isn't queried.
	if s := a.Status(context.Background(), &clientv1.Zrepl{}); len(s.Jobs) != 0 || s.Error != "" {
		t.Errorf("empty status = %v", s)
	}
}
