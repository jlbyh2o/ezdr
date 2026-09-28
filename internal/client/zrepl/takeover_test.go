package zrepl

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// A main configuration with hand-written jobs and no include, as before a
// takeover.
const oldMain = `global:
  logging:
  - type: syslog
    format: human
    level: info

jobs:
- name: pve1_source
  type: source
  # Replicates the whole pool.
  filesystems:
    "rpool<": true
- name: keep_snap
  type: snap
`

func TestRemoveAndRestore(t *testing.T) {
	ctx := context.Background()
	p := testPaths(t)
	if err := os.WriteFile(p.MainConfig, []byte(oldMain), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{}
	a := &Applier{Paths: p, Run: f.run}

	if err := a.RemoveJobs(ctx, []string{"pve1_source"}, "abc123"); err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Global  map[string]any   `yaml:"global"`
		Jobs    []map[string]any `yaml:"jobs"`
		Include []string         `yaml:"include"`
	}
	b, _ := os.ReadFile(p.MainConfig)
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Jobs) != 1 || cfg.Jobs[0]["name"] != "keep_snap" || cfg.Global == nil ||
		len(cfg.Include) != 1 || cfg.Include[0] != p.JobsDir+"/" {
		t.Errorf("edited configuration:\n%s", b)
	}
	if _, err := os.Stat(p.JobsFile()); err != nil {
		t.Error("jobs file not created before the include")
	}
	if !f.did("zrepl configcheck") || !f.did("systemctl restart zrepl") {
		t.Errorf("calls = %v", f.calls)
	}
	backup := p.MainConfig + ".ezdr-takeover-abc123"
	if got, _ := os.ReadFile(backup); string(got) != oldMain { //nolint:gosec // test file
		t.Errorf("backup = %q", got)
	}

	// A retry keeps the original backup and changes nothing.
	if err := a.RemoveJobs(ctx, []string{"pve1_source"}, "abc123"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(backup); string(got) != oldMain { //nolint:gosec // test file
		t.Error("retry replaced the backup")
	}

	if err := a.RestoreConfig(ctx, "abc123"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p.MainConfig); string(got) != oldMain {
		t.Errorf("restored = %q", got)
	}
	// The original didn't include EZDR's jobs, so the empty jobs file goes
	// too; otherwise applying desired state would add the include back.
	if _, err := os.Stat(p.JobsFile()); !os.IsNotExist(err) {
		t.Errorf("jobs file left after restore: %v", err)
	}
	if err := a.Apply(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p.MainConfig); string(got) != oldMain {
		t.Error("applying empty desired state after a restore changed the configuration")
	}
	// Without a backup there's nothing to restore.
	if err := a.RestoreConfig(ctx, "other"); err != nil {
		t.Errorf("restore without backup: %v", err)
	}
	if err := a.RestoreConfig(ctx, "../etc"); err == nil {
		t.Error("accepted an invalid backup name")
	}
}

func TestRemoveJobsRejected(t *testing.T) {
	ctx := context.Background()
	p := testPaths(t)
	if err := os.WriteFile(p.MainConfig, []byte(oldMain), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{failCheck: true}
	a := &Applier{Paths: p, Run: f.run}
	if err := a.RemoveJobs(ctx, []string{"pve1_source"}, "x1"); err == nil || !strings.Contains(err.Error(), "original kept") {
		t.Errorf("err = %v", err)
	}
	if got, _ := os.ReadFile(p.MainConfig); string(got) != oldMain {
		t.Error("rejected edit was left in place")
	}
	if f.did("systemctl restart") {
		t.Error("zrepl restarted after a rejected edit")
	}

	// Jobs in other files aren't edited.
	other := filepath.Join(filepath.Dir(p.MainConfig), "other.yml")
	if err := os.WriteFile(other, []byte("jobs:\n- name: elsewhere\n  type: snap\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.MainConfig, []byte(oldMain+"include:\n  - "+other+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.Run = (&fakeRunner{}).run
	if err := a.RemoveJobs(ctx, []string{"elsewhere"}, "x2"); err == nil || !strings.Contains(err.Error(), "only jobs in") {
		t.Errorf("err = %v", err)
	}
}

func TestPreflight(t *testing.T) {
	ds := "rpool/vm-101-disk-0"
	f := &fakeRunner{out: map[string]string{
		"zfs list -Hp -t filesystem,volume,snapshot,bookmark -d 1 -o name,guid,createtxg,referenced " + ds: "" +
			ds + "\t1\t10\t4096\n" +
			ds + "@zrepl_2\t30\t300\t4096\n" +
			ds + "#zrepl_CURSOR_G_x_J_pve1_source\t20\t200\t4096\n" +
			ds + "@zrepl_1\t20\t200\t4096\n",
		"zrepl zfs-abstraction release-all --job pve1_source --dry-run": "would destroy x\nwould destroy y\n",
	}}
	a := &Applier{Paths: testPaths(t), Run: f.run}
	res, err := a.Preflight(context.Background(), []string{ds, "rpool/vm-999-disk-0"}, "pve1_source")
	if err != nil {
		t.Fatal(err)
	}
	if res.ZreplVersion != "v0.7.0" || !res.ZreplRunning || len(res.ReleasePreview) != 2 || len(res.Datasets) != 2 {
		t.Fatalf("result = %v", res)
	}
	d := res.Datasets[0]
	if !d.Exists || d.ReferencedBytes != 4096 || len(d.Snapshots) != 3 || d.Snapshots[0].Guid != 20 || d.Snapshots[2].Name != "@zrepl_2" {
		t.Errorf("dataset = %v", d)
	}
	// The fake reports no such dataset for anything it doesn't know.
	if res.Datasets[1].Exists {
		t.Errorf("missing dataset = %v", res.Datasets[1])
	}
	if _, err := a.Preflight(context.Background(), nil, "bad job; rm"); err == nil {
		t.Error("accepted an invalid job name")
	}
}

// An upgrade repeated after an interrupted attempt (0.7 already unpacked)
// still finishes dpkg's work, holds the package, and restarts zrepl.
func TestUpgradeRepeat(t *testing.T) {
	f := &fakeRunner{}
	a := &Applier{Paths: testPaths(t), Run: f.run}
	v, err := a.Upgrade(context.Background())
	if err != nil || v != "v0.7.0" {
		t.Fatalf("Upgrade = %q, %v", v, err)
	}
	for _, want := range []string{"dpkg --force-confdef --force-confold --configure -a", "apt-mark hold zrepl", "systemctl restart zrepl"} {
		if !f.did(want) {
			t.Errorf("missing %q in %v", want, f.calls)
		}
	}
	if f.did("apt-get install") {
		t.Error("reinstalled an already supported zrepl")
	}
}
