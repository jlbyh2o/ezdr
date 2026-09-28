package failback

import (
	"context"
	"crypto/tls"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jlbyh2o/ezdr/internal/client/zrepl"
	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
)

// host is a fake host: zfs send writes size bytes per replica, zfs receive
// stores what it reads in dir, and snapshots have the GUIDs in guids.
type host struct {
	t    *testing.T
	dir  string
	pem  string
	size int
	// failReceive makes zfs receive fail after reading a little.
	failReceive bool
	guids       map[string]string

	mu   sync.Mutex
	cmds []string
}

func newHost(t *testing.T, name string) *host {
	t.Helper()
	dialRetry = 10 * time.Millisecond
	dir := t.TempDir()
	pem, err := zrepl.EnsureCertificate(zrepl.Paths{CertDir: dir}, name)
	if err != nil {
		t.Fatal(err)
	}
	return &host{t: t, dir: dir, pem: pem, size: 2*maxFrame + 123, guids: map[string]string{}}
}

func (h *host) record(name string, args []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cmds = append(h.cmds, name+" "+strings.Join(args, " "))
}

func (h *host) did() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.cmds, "|")
}

func (h *host) transfer() *Transfer {
	return &Transfer{
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			h.record(name, args)
			if args[0] == "get" {
				g, ok := h.guids[args[len(args)-1]]
				if !ok {
					g = "42"
				}
				return []byte(g + "\n"), nil
			}
			return nil, nil
		},
		Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			h.record(name, args)
			script := "head -c " + strconv.Itoa(h.size) + " /dev/zero"
			if args[0] == "receive" {
				script = "cat > " + filepath.Join(h.dir, strings.ReplaceAll(args[len(args)-1], "/", "_"))
				if h.failReceive {
					script = "head -c 10 >/dev/null; echo 'cannot receive: bad stream' >&2; exit 1"
				}
			}
			return exec.CommandContext(ctx, "sh", "-c", script) //nolint:gosec // test commands
		},
		Cert: func() (tls.Certificate, error) {
			p := zrepl.Paths{CertDir: h.dir}
			return tls.LoadX509KeyPair(p.CertFile(), p.KeyFile())
		},
	}
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close() //nolint:errcheck // test
	return ln.Addr().String()
}

type round struct {
	recv, send       *clientv1.FailbackTransfer
	recvErr, sendErr error
}

// runRound runs a round between primary and dr, with dr sending sources.
func runRound(t *testing.T, primary, dr *host, targets []*clientv1.FailbackTarget, sources []*clientv1.FailbackSource) round {
	t.Helper()
	addr := freePort(t)
	var r round
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.recv, r.recvErr = primary.transfer().Receive(context.Background(), &clientv1.FailbackReceive{
			ListenAddress: addr, Peer: &clientv1.Peer{CertificatePem: dr.pem}, Datasets: targets, ConnectTimeoutSeconds: 10})
	}()
	r.send, r.sendErr = dr.transfer().Send(context.Background(), &clientv1.FailbackSend{
		Address: addr, Peer: &clientv1.Peer{CertificatePem: primary.pem}, Datasets: sources})
	wg.Wait()
	return r
}

func TestRound(t *testing.T) {
	primary, dr := newHost(t, "ezdr-p"), newHost(t, "ezdr-d")
	targets := []*clientv1.FailbackTarget{
		{Dataset: "rpool/data/vm-201-disk-0", FromSnapshot: "zrepl_1"},
		{Dataset: "rpool/data/subvol-101-disk-0", FromSnapshot: "zrepl_1", Rollback: true},
	}
	sources := []*clientv1.FailbackSource{
		{Replica: "tank/r/rpool/data/vm-201-disk-0", Dataset: "rpool/data/vm-201-disk-0", FromSnapshot: "zrepl_1", ToSnapshot: "zrepl_2"},
		{Replica: "tank/r/rpool/data/subvol-101-disk-0", Dataset: "rpool/data/subvol-101-disk-0", FromSnapshot: "zrepl_1", ToSnapshot: "zrepl_2", Raw: true},
	}
	r := runRound(t, primary, dr, targets, sources)
	if r.recvErr != nil || r.sendErr != nil {
		t.Fatalf("receive: %v, send: %v", r.recvErr, r.sendErr)
	}
	want := uint64(primary.size) //nolint:gosec // small positive size
	for _, res := range []*clientv1.FailbackTransfer{r.recv, r.send} {
		if len(res.Datasets) != 2 || res.Datasets[0].Bytes != want || res.Datasets[1].Bytes != want {
			t.Errorf("transfer = %v", res)
		}
	}
	b, err := os.ReadFile(filepath.Join(primary.dir, "rpool_data_vm-201-disk-0"))
	if err != nil || len(b) != primary.size {
		t.Errorf("received %d bytes, %v", len(b), err)
	}
	for _, c := range []struct {
		h    *host
		want []string
	}{
		{primary, []string{
			"zfs receive rpool/data/vm-201-disk-0",
			"zfs get -Hp -o value guid rpool/data/vm-201-disk-0@zrepl_2",
			"zfs rollback -r rpool/data/subvol-101-disk-0@zrepl_1|zfs receive -F rpool/data/subvol-101-disk-0",
		}},
		{dr, []string{
			"zfs send -L -c -e -I tank/r/rpool/data/vm-201-disk-0@zrepl_1 tank/r/rpool/data/vm-201-disk-0@zrepl_2",
			"zfs send -w -I tank/r/rpool/data/subvol-101-disk-0@zrepl_1 tank/r/rpool/data/subvol-101-disk-0@zrepl_2",
		}},
	} {
		if got := c.h.did(); !containsAll(got, c.want) {
			t.Errorf("commands = %s, want %v", got, c.want)
		}
	}
}

func containsAll(s string, subs []string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

func TestRoundFailures(t *testing.T) {
	target := []*clientv1.FailbackTarget{{Dataset: "rpool/data/vm-201-disk-0", FromSnapshot: "zrepl_1"}}
	source := func(from string) []*clientv1.FailbackSource {
		return []*clientv1.FailbackSource{{Replica: "tank/r/rpool/data/vm-201-disk-0", Dataset: "rpool/data/vm-201-disk-0",
			FromSnapshot: from, ToSnapshot: "zrepl_2"}}
	}
	for _, c := range []struct {
		name     string
		setup    func(primary, dr *host)
		from     string
		recvWant string
		sendWant string
	}{
		{"wrong start", nil, "zrepl_0", `start from "zrepl_0", not "zrepl_1"`, `the primary: the changes start from "zrepl_0"`},
		{"receive fails", func(p, _ *host) { p.failReceive = true }, "zrepl_1", "cannot receive: bad stream", "the primary: zfs receive stopped: cannot receive: bad stream"},
		{"GUID mismatch", func(p, _ *host) { p.guids["rpool/data/vm-201-disk-0@zrepl_2"] = "7" }, "zrepl_1",
			"GUID doesn't match", "the primary: received @zrepl_2, but its GUID"},
	} {
		t.Run(c.name, func(t *testing.T) {
			primary, dr := newHost(t, "ezdr-p"), newHost(t, "ezdr-d")
			if c.setup != nil {
				c.setup(primary, dr)
			}
			r := runRound(t, primary, dr, target, source(c.from))
			if r.recvErr == nil || !strings.Contains(r.recvErr.Error(), c.recvWant) {
				t.Errorf("receive error = %v, want %q", r.recvErr, c.recvWant)
			}
			if r.sendErr == nil || !strings.Contains(r.sendErr.Error(), c.sendWant) {
				t.Errorf("send error = %v, want %q", r.sendErr, c.sendWant)
			}
		})
	}
}

// TestImpostor checks that the primary refuses a host with the wrong
// certificate and keeps waiting for the DR host.
func TestImpostor(t *testing.T) {
	primary, dr, other := newHost(t, "ezdr-p"), newHost(t, "ezdr-d"), newHost(t, "ezdr-x")
	addr := freePort(t)
	target := []*clientv1.FailbackTarget{{Dataset: "rpool/data/vm-201-disk-0", FromSnapshot: "zrepl_1"}}
	src := []*clientv1.FailbackSource{{Replica: "tank/r/rpool/data/vm-201-disk-0", Dataset: "rpool/data/vm-201-disk-0",
		FromSnapshot: "zrepl_1", ToSnapshot: "zrepl_2"}}
	done := make(chan error, 1)
	go func() {
		_, err := primary.transfer().Receive(context.Background(), &clientv1.FailbackReceive{
			ListenAddress: addr, Peer: &clientv1.Peer{CertificatePem: dr.pem}, Datasets: target, ConnectTimeoutSeconds: 10})
		done <- err
	}()
	if _, err := other.transfer().Send(context.Background(), &clientv1.FailbackSend{
		Address: addr, Peer: &clientv1.Peer{CertificatePem: primary.pem}, Datasets: src}); err == nil {
		t.Error("an impostor sent a round")
	}
	// The DR host doesn't accept a primary with the wrong certificate either.
	if _, err := dr.transfer().Send(context.Background(), &clientv1.FailbackSend{
		Address: addr, Peer: &clientv1.Peer{CertificatePem: other.pem}, Datasets: src}); err == nil ||
		!strings.Contains(err.Error(), "isn't the expected one") {
		t.Errorf("sent to an impostor primary: %v", err)
	}
	if _, err := dr.transfer().Send(context.Background(), &clientv1.FailbackSend{
		Address: addr, Peer: &clientv1.Peer{CertificatePem: primary.pem}, Datasets: src}); err != nil {
		t.Errorf("send: %v", err)
	}
	if err := <-done; err != nil {
		t.Errorf("receive: %v", err)
	}
	if strings.Contains(primary.did(), "receive") && strings.Count(primary.did(), "zfs receive") != 1 {
		t.Errorf("primary commands = %s", primary.did())
	}
}

func TestReceiveTimeout(t *testing.T) {
	primary, dr := newHost(t, "ezdr-p"), newHost(t, "ezdr-d")
	_, err := primary.transfer().Receive(context.Background(), &clientv1.FailbackReceive{
		ListenAddress: freePort(t), Peer: &clientv1.Peer{CertificatePem: dr.pem}, ConnectTimeoutSeconds: 1})
	if err == nil || !strings.Contains(err.Error(), "didn't connect within 1s") {
		t.Errorf("err = %v", err)
	}
}
