package cleanup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
)

// fake answers commands from canned outputs, keyed by the whole command
// line, and records every call.
type fake struct {
	out   map[string]string
	calls []string
}

func (f *fake) run(_ context.Context, name string, args ...string) ([]byte, error) {
	cmd := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, cmd)
	if out, ok := f.out[cmd]; ok {
		return []byte(out), nil
	}
	if strings.HasPrefix(cmd, "zfs destroy") || strings.HasPrefix(cmd, "zrepl ") || strings.HasPrefix(cmd, "zfs holds") {
		return nil, nil
	}
	return nil, errors.New("unexpected command: " + cmd)
}

func testRunner(t *testing.T) (*Runner, *fake) {
	t.Helper()
	pve := t.TempDir()
	write := func(path, data string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(pve, "storage.cfg"), "dir: local\n\tpath /var/lib/vz\n\nzfspool: local-zfs\n\tpool rpool/data\n\tsparse 1\n\n"+
		"zfspool: ezdr-test-tank\n\tpool tank/ezdr-test\n")
	write(filepath.Join(pve, "nodes", "dr", "qemu-server", "300.conf"), "name: other\nscsi0: local-zfs:vm-300-disk-0,size=4G\nnet0: virtio=BC:24:11:48:39:FB,bridge=vmbr1\n")
	f := &fake{out: map[string]string{}}
	return &Runner{PVE: pve, Run: f.run}, f
}

func TestScanAndCleanReplicas(t *testing.T) {
	r, f := testRunner(t)
	f.out["zfs list -Hp -t filesystem,volume -o name,used,origin"] = "tank\t100\t-\n" +
		"tank/ezdr/pve1\t10\t-\ntank/ezdr/pve1/rpool\t10\t-\ntank/ezdr/pve1/rpool/data\t10\t-\n" +
		"tank/ezdr/pve1/rpool/data/vm-101-disk-0\t4096\t-\n" +
		"tank/ezdr/pve1/rpool/data/vm-102-disk-0\t2048\t-\n" +
		"tank/ezdr-test/vm-102-disk-0\t1\ttank/ezdr/pve1/rpool/data/vm-102-disk-0@ezdr_2\n"
	f.out["zfs list -Hp -t snapshot,bookmark -o name,clones -r tank/ezdr/pve1/rpool/data/vm-101-disk-0"] =
		"tank/ezdr/pve1/rpool/data/vm-101-disk-0@ezdr_1\t-\ntank/ezdr/pve1/rpool/data/vm-101-disk-0@ezdr_2\t-\n"
	f.out["zfs holds -H tank/ezdr/pve1/rpool/data/vm-101-disk-0@ezdr_1 tank/ezdr/pve1/rpool/data/vm-101-disk-0@ezdr_2"] =
		"tank/ezdr/pve1/rpool/data/vm-101-disk-0@ezdr_2\tzrepl_last_received_J_ezdr_abc_local-zfs_pull\tTue\n"
	f.out["zfs list -Hp -t snapshot,bookmark -o name,clones -r tank/ezdr/pve1/rpool/data/vm-102-disk-0"] =
		"tank/ezdr/pve1/rpool/data/vm-102-disk-0@ezdr_2\ttank/ezdr-test/vm-102-disk-0\n"

	targets := []*clientv1.DataTarget{
		{Dataset: "tank/ezdr/pve1/rpool/data/vm-101-disk-0", Destroy: true, EmptyParentsBelow: "tank/ezdr/pve1"},
		{Dataset: "tank/ezdr/pve1/rpool/data/vm-102-disk-0", Destroy: true, EmptyParentsBelow: "tank/ezdr/pve1"},
		{Dataset: "tank/ezdr/pve1/rpool/data/vm-103-disk-0", Destroy: true},
		{Dataset: "tank", Destroy: true},
	}
	jobs := []string{"ezdr_abc_local-zfs_pull"}
	res, err := r.Scan(context.Background(), targets, jobs)
	if err != nil {
		t.Fatal(err)
	}
	d := res.Datasets
	if !d[0].Exists || d[0].ReclaimBytes != 4096 || d[0].Snapshots != 2 || len(d[0].Problems) != 0 {
		t.Errorf("replica = %v", d[0])
	}
	if len(d[1].Problems) != 1 || !strings.Contains(d[1].Problems[0], "tank/ezdr-test/vm-102-disk-0 is a clone") {
		t.Errorf("cloned replica = %v", d[1])
	}
	if d[2].Exists || len(d[2].Problems) != 0 {
		t.Errorf("missing replica = %v", d[2])
	}
	if len(d[3].Problems) != 1 || d[3].Problems[0] != "a pool can't be destroyed" {
		t.Errorf("pool = %v", d[3])
	}

	// A problem anywhere blocks the whole cleanup.
	f.calls = nil
	if _, err := r.Clean(context.Background(), targets, jobs); err == nil || !strings.Contains(err.Error(), "is a clone") {
		t.Fatalf("clean with problems: err = %v", err)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "zfs destroy") || strings.HasPrefix(c, "zrepl") {
			t.Errorf("changed something despite problems: %s", c)
		}
	}

	// Without the cloned replica, it releases the job, destroys the replica,
	// and removes the parents left empty.
	f.calls = nil
	f.out["zfs list -H -t all -o name -d 1 tank/ezdr/pve1/rpool/data"] = "tank/ezdr/pve1/rpool/data\n"
	f.out["zfs list -H -t all -o name -d 1 tank/ezdr/pve1/rpool"] = "tank/ezdr/pve1/rpool\n"
	events, err := r.Clean(context.Background(), targets[:1], jobs)
	if err != nil {
		t.Fatal(err)
	}
	var changes []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "zfs destroy") || strings.HasPrefix(c, "zrepl") {
			changes = append(changes, c)
		}
	}
	want := []string{
		"zrepl zfs-abstraction release-all --job ezdr_abc_local-zfs_pull",
		"zfs destroy -r tank/ezdr/pve1/rpool/data/vm-101-disk-0",
		"zfs destroy tank/ezdr/pve1/rpool/data",
		"zfs destroy tank/ezdr/pve1/rpool",
	}
	if strings.Join(changes, "\n") != strings.Join(want, "\n") {
		t.Errorf("changes:\n%s\nwant:\n%s", strings.Join(changes, "\n"), strings.Join(want, "\n"))
	}
	if len(events) != 4 || events[1] != "destroyed tank/ezdr/pve1/rpool/data/vm-101-disk-0 (4.0 KiB)" {
		t.Errorf("events = %q", events)
	}
}

func TestReplicaInUse(t *testing.T) {
	r, f := testRunner(t)
	f.out["zfs list -Hp -t filesystem,volume -o name,used,origin"] = "rpool/data/vm-300-disk-0\t1\t-\ntank/ezdr-test\t1\t-\n"
	f.out["zfs list -Hp -t snapshot,bookmark -o name,clones -r rpool/data/vm-300-disk-0"] = ""
	f.out["zfs list -Hp -t snapshot,bookmark -o name,clones -r tank/ezdr-test"] = ""
	res, err := r.Scan(context.Background(), []*clientv1.DataTarget{
		{Dataset: "rpool/data/vm-300-disk-0", Destroy: true},
		{Dataset: "tank/ezdr-test", Destroy: true},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p := res.Datasets[0].Problems; len(p) != 1 || p[0] != "used by guest 300 (rpool/data/vm-300-disk-0)" {
		t.Errorf("guest disk problems = %q", p)
	}
	if p := res.Datasets[1].Problems; len(p) != 1 || p[0] != "used by storage ezdr-test-tank" {
		t.Errorf("storage problems = %q", p)
	}
}

func TestCleanSnapshots(t *testing.T) {
	r, f := testRunner(t)
	f.out["zfs list -Hp -t filesystem,volume -o name,used,origin"] = "rpool/data/vm-101-disk-0\t5000\t-\n"
	f.out["zfs list -Hp -t snapshot,bookmark -o name,clones -d 1 rpool/data/vm-101-disk-0"] =
		"rpool/data/vm-101-disk-0@zrepl_1\t-\n" +
			"rpool/data/vm-101-disk-0@zrepl_2\t-\n" +
			"rpool/data/vm-101-disk-0@zrepl_3\trpool/data/vm-999-disk-0\n" +
			"rpool/data/vm-101-disk-0@backup_1\t-\n" +
			"rpool/data/vm-101-disk-0@zrepl_4\t-\n" +
			"rpool/data/vm-101-disk-0#zrepl_2\t-\n" +
			"rpool/data/vm-101-disk-0#zrepl_CURSOR_G_1_J_ezdr_abc_local-zfs\t-\n" +
			"rpool/data/vm-101-disk-0#other\t-\n"
	f.out["zfs holds -H rpool/data/vm-101-disk-0@zrepl_1 rpool/data/vm-101-disk-0@zrepl_2 rpool/data/vm-101-disk-0@zrepl_3 rpool/data/vm-101-disk-0@zrepl_4"] =
		"rpool/data/vm-101-disk-0@zrepl_4\tvzdump\tTue\n"
	f.out["zfs destroy -nvp rpool/data/vm-101-disk-0@zrepl_1,zrepl_2"] = "destroy\trpool/data/vm-101-disk-0@zrepl_1\ndestroy\trpool/data/vm-101-disk-0@zrepl_2\nreclaim\t3000\n"
	// The cursor is gone once the job is released.
	f.out["zfs list -H -t all -o name rpool/data/vm-101-disk-0#zrepl_CURSOR_G_1_J_ezdr_abc_local-zfs"] = ""

	targets := []*clientv1.DataTarget{{Dataset: "rpool/data/vm-101-disk-0", Prefix: "zrepl_"}}
	jobs := []string{"ezdr_abc_local-zfs"}
	res, err := r.Scan(context.Background(), targets, jobs)
	if err != nil {
		t.Fatal(err)
	}
	s := res.Datasets[0]
	if s.Snapshots != 2 || s.Bookmarks != 2 || s.ReclaimBytes != 3000 || len(s.Problems) != 0 ||
		strings.Join(s.Skipped, "|") != "rpool/data/vm-101-disk-0@zrepl_3 (has clones)|rpool/data/vm-101-disk-0@zrepl_4 (held by vzdump)" {
		t.Errorf("scan = %v", s)
	}

	f.calls = nil
	destroyCursor := "zfs destroy rpool/data/vm-101-disk-0#zrepl_CURSOR_G_1_J_ezdr_abc_local-zfs"
	r.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if cmd := name + " " + strings.Join(args, " "); cmd == destroyCursor {
			f.calls = append(f.calls, cmd)
			return nil, errors.New("dataset does not exist")
		} else if cmd == "zfs list -H -t all -o name rpool/data/vm-101-disk-0#zrepl_CURSOR_G_1_J_ezdr_abc_local-zfs" {
			f.calls = append(f.calls, cmd)
			return nil, errors.New("dataset does not exist")
		}
		return f.run(ctx, name, args...)
	}
	events, err := r.Clean(context.Background(), targets, jobs)
	if err != nil {
		t.Fatal(err)
	}
	var changes []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "zfs destroy") && !strings.Contains(c, "-nvp") || strings.HasPrefix(c, "zrepl") {
			changes = append(changes, c)
		}
	}
	want := []string{
		"zrepl zfs-abstraction release-all --job ezdr_abc_local-zfs",
		"zfs destroy rpool/data/vm-101-disk-0@zrepl_1,zrepl_2",
		"zfs destroy rpool/data/vm-101-disk-0#zrepl_2",
		destroyCursor,
	}
	if strings.Join(changes, "\n") != strings.Join(want, "\n") {
		t.Errorf("changes:\n%s\nwant:\n%s", strings.Join(changes, "\n"), strings.Join(want, "\n"))
	}
	if events[len(events)-1] != "deleted 2 snapshot(s) and 1 bookmark(s) on rpool/data/vm-101-disk-0 (2.9 KiB)" {
		t.Errorf("events = %q", events)
	}
}
