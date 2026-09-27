package zrepl

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	inventoryv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/inventory/v1"
)

// A hand-written setup like the ones takeover adopts: jobs in the main file,
// plus EZDR's include directory.
const handWritten = `global:
  logging:
  - type: syslog
    format: human
    level: info
include:
  - %s/
jobs:
- name: pve1_source
  type: source
  serve:
    type: tls
    listen: "192.0.2.12:8888"
    listen_freebind: true
    ca: /etc/zrepl/certs/ca.crt
    cert: /etc/zrepl/certs/pve1.crt
    key: /etc/zrepl/certs/pve1.key
    client_cns:
    - dr1
  filesystems:
    "rpool<": true
    # Not replicated
    "rpool/vm-211-disk-0": false
  snapshotting:
    type: periodic
    prefix: zrepl_
    interval: 5m
  send:
    encrypted: false
    compressed: true
    large_blocks: true
    embedded_data: true
- name: dr1_pull
  type: pull
  connect:
    type: tls
    address: "192.0.2.12:8888"
    server_cn: pve1
  root_fs: "tank/replicated"
  interval: 1d
  pruning:
    keep_sender:
    - type: not_replicated
    - type: grid
      grid: 1x1h(keep=all) | 24x1h | 7x1d
      regex: "^zrepl_"
    keep_receiver:
    - type: regex
      negate: true
      regex: "^zrepl_"
`

func TestReadJobs(t *testing.T) {
	p := testPaths(t)
	if jobs, err := ReadJobs(p); jobs != nil || err != nil {
		t.Fatalf("missing config: got %v, %v", jobs, err)
	}
	if err := os.WriteFile(p.MainConfig, []byte(strings.Replace(handWritten, "%s", p.JobsDir, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(p.JobsDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.JobsFile(), []byte("jobs:\n- name: ezdr_x\n  type: snap\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Not a configuration file.
	if err := os.WriteFile(filepath.Join(p.JobsDir, "README"), []byte("x: ["), 0o600); err != nil {
		t.Fatal(err)
	}

	jobs, err := ReadJobs(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 3 {
		t.Fatalf("got %d jobs, want 3", len(jobs))
	}
	want := &inventoryv1.ZreplJob{
		Name: "pve1_source", Type: "source", File: p.MainConfig, Transport: "tls",
		ListenAddress: "192.0.2.12:8888", ListenFreebind: true, ClientCns: []string{"dr1"},
		Filesystems: []*inventoryv1.ZreplFilter{
			{Pattern: "rpool<", Include: true}, {Pattern: "rpool/vm-211-disk-0", Include: false},
		},
		SnapshottingType: "periodic", SnapshotPrefix: "zrepl_", SnapshotIntervalSeconds: 300,
		Send:       &inventoryv1.ZreplSend{Compressed: true, LargeBlocks: true, EmbeddedData: true},
		KeepSender: []*inventoryv1.ZreplPruneRule{}, KeepReceiver: []*inventoryv1.ZreplPruneRule{},
	}
	if !proto.Equal(jobs[0], want) {
		t.Errorf("source job:\n got %v\nwant %v", jobs[0], want)
	}
	pull := jobs[1]
	if pull.ConnectAddress != "192.0.2.12:8888" || pull.ServerCn != "pve1" || pull.RootFs != "tank/replicated" ||
		pull.IntervalSeconds != 86400 || len(pull.KeepSender) != 2 || pull.KeepSender[1].Grid != "1x1h(keep=all) | 24x1h | 7x1d" ||
		!pull.KeepReceiver[0].Negate {
		t.Errorf("pull job: %v", pull)
	}
	if jobs[2].Name != "ezdr_x" || !jobs[2].Managed || jobs[0].Managed {
		t.Errorf("managed flags: %v, %v", jobs[0].Managed, jobs[2])
	}

	// A broken included file is reported; the other jobs remain.
	if err := os.WriteFile(filepath.Join(p.JobsDir, "broken.yml"), []byte("jobs: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	jobs, err = ReadJobs(p)
	if err == nil || len(jobs) != 3 {
		t.Errorf("broken include: got %d jobs, err %v", len(jobs), err)
	}
}

func TestDetect(t *testing.T) {
	p := testPaths(t)
	f := &fakeRunner{}
	a := &Applier{Paths: p, Run: f.run}
	z := a.Detect(context.Background())
	if z.Version != "v0.7.0" || !z.Running || z.ConfigError != "" || len(z.Jobs) != 0 {
		t.Errorf("Detect = %v", z)
	}
	f.notRunning = true
	if a.Detect(context.Background()).Running {
		t.Error("Detect reported a stopped zrepl as running")
	}
}

func TestSeconds(t *testing.T) {
	for in, want := range map[string]uint32{"5m": 300, "1h30m": 5400, "14d": 14 * 86400, "manual": 0, "": 0, "-5m": 0} {
		if got := seconds(in); got != want {
			t.Errorf("seconds(%q) = %d, want %d", in, got, want)
		}
	}
}
