// Package failback copies a failed-over plan's changes from the DR host back
// to the primary over mutual TLS, one round at a time. See
// docs/design/failback.md, section 2.
//
// The DR host connects to the primary and, for each dataset, sends a header
// frame, the output of `zfs send -I` in data frames, and an end frame. The
// primary receives it with `zfs receive`, checks the result, and answers
// with a result frame. A quit frame ends the round.
package failback

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jlbyh2o/ezdr/internal/client/zrepl"
	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
)

// Frame types.
const (
	frameHeader = 'H' // DR host: a dataset's header (JSON)
	frameData   = 'D' // DR host: part of the zfs send stream
	frameEnd    = 'E' // DR host: the stream ended; the payload is an error, empty on success
	frameResult = 'R' // primary: a dataset's result (JSON)
	frameQuit   = 'Q' // DR host: no more datasets
)

const (
	maxFrame = 1 << 20
	// idleTimeout bounds each read and write on the connection.
	idleTimeout = 10 * time.Minute
	// dialTimeout is how long the DR host tries to reach the primary, which
	// starts listening at about the same time.
	dialTimeout = 2 * time.Minute
)

// dialRetry is how long the DR host waits between attempts (shortened in
// tests).
var dialRetry = 2 * time.Second

var (
	datasetPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]*(/[A-Za-z0-9_.:-]+)+$`)
	snapshotPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]+$`)
)

type header struct {
	Dataset string `json:"dataset"`
	From    string `json:"from"`
	To      string `json:"to"`
	// ToGUID is the DR host's GUID of the "to" snapshot; the primary checks
	// it received the same snapshot.
	ToGUID uint64 `json:"to_guid"`
}

type result struct {
	Bytes uint64 `json:"bytes"`
	Error string `json:"error,omitempty"`
}

// Transfer sends and receives failback rounds.
type Transfer struct {
	// Run runs a command and returns its output.
	Run func(ctx context.Context, name string, args ...string) ([]byte, error)
	// Command prepares zfs send and zfs receive.
	Command func(ctx context.Context, name string, args ...string) *exec.Cmd
	// Cert returns this host's TLS certificate and key.
	Cert func() (tls.Certificate, error)
}

// NewTransfer returns a Transfer for the real host, using its zrepl
// certificate.
func NewTransfer() *Transfer {
	p := zrepl.DefaultPaths
	return &Transfer{
		Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			out, err := exec.CommandContext(ctx, name, args...).CombinedOutput() //nolint:gosec // fixed commands with validated arguments
			if err != nil {
				return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
			}
			return out, nil
		},
		Command: exec.CommandContext,
		Cert:    func() (tls.Certificate, error) { return tls.LoadX509KeyPair(p.CertFile(), p.KeyFile()) },
	}
}

// Receive listens for the DR host, receives one round, and stops listening.
// Connections from anyone else are refused without ending the wait.
func (t *Transfer) Receive(ctx context.Context, m *clientv1.FailbackReceive) (*clientv1.FailbackTransfer, error) {
	targets := map[string]*clientv1.FailbackTarget{}
	for _, d := range m.Datasets {
		if !datasetPattern.MatchString(d.Dataset) || !snapshotPattern.MatchString(d.FromSnapshot) {
			return nil, fmt.Errorf("invalid dataset or snapshot %q@%q", d.Dataset, d.FromSnapshot)
		}
		targets[d.Dataset] = d
	}
	cfg, err := t.tlsConfig(m.GetPeer().GetCertificatePem())
	if err != nil {
		return nil, err
	}
	cfg.ClientAuth = tls.RequireAnyClientCert

	lc := net.ListenConfig{}
	if m.ListenFreebind {
		lc.Control = freebind
	}
	ln, err := lc.Listen(ctx, "tcp", m.ListenAddress)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", m.ListenAddress, err)
	}
	wait := time.Duration(m.ConnectTimeoutSeconds) * time.Second
	actx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	go func() {
		<-actx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if actx.Err() != nil {
				return nil, fmt.Errorf("the DR host didn't connect within %s", wait)
			}
			return nil, err
		}
		tc := tls.Server(conn, cfg)
		hctx, hcancel := context.WithTimeout(actx, 30*time.Second)
		err = tc.HandshakeContext(hctx)
		hcancel()
		if err != nil {
			slog.Warn("refused a failback connection", "from", conn.RemoteAddr(), "err", err)
			_ = conn.Close()
			continue
		}
		_ = ln.Close()
		defer tc.Close() //nolint:errcheck // done with the connection
		return t.serve(ctx, newFramer(tc), targets)
	}
}

// serve receives the datasets the DR host sends until it quits.
func (t *Transfer) serve(ctx context.Context, f *framer, targets map[string]*clientv1.FailbackTarget) (*clientv1.FailbackTransfer, error) {
	out := &clientv1.FailbackTransfer{}
	done := map[string]bool{}
	for {
		typ, payload, err := f.read()
		if err != nil {
			return out, fmt.Errorf("connection to the DR host: %w", err)
		}
		switch typ {
		case frameQuit:
			for ds := range targets {
				if !done[ds] {
					return out, fmt.Errorf("the DR host didn't send %s", ds)
				}
			}
			return out, nil
		case frameHeader:
			var h header
			if err := json.Unmarshal(payload, &h); err != nil {
				return out, fmt.Errorf("bad header from the DR host: %w", err)
			}
			n, err := t.receiveOne(ctx, f, targets, done, h)
			if err != nil {
				b, _ := json.Marshal(result{Bytes: n, Error: err.Error()})
				_ = f.write(frameResult, b)
				return out, fmt.Errorf("%s: %w", h.Dataset, err)
			}
			done[h.Dataset] = true
			out.Datasets = append(out.Datasets, &clientv1.DatasetTransfer{Dataset: h.Dataset, Bytes: n})
			b, _ := json.Marshal(result{Bytes: n})
			if err := f.write(frameResult, b); err != nil {
				return out, fmt.Errorf("connection to the DR host: %w", err)
			}
		default:
			return out, fmt.Errorf("unexpected frame %q from the DR host", typ)
		}
	}
}

// receiveOne receives one dataset's stream and checks that the snapshot it
// ends with arrived.
func (t *Transfer) receiveOne(ctx context.Context, f *framer, targets map[string]*clientv1.FailbackTarget, done map[string]bool, h header) (uint64, error) {
	tg := targets[h.Dataset]
	switch {
	case tg == nil:
		return 0, errors.New("not a dataset of this failback")
	case done[h.Dataset]:
		return 0, errors.New("sent twice")
	case h.From != tg.FromSnapshot:
		return 0, fmt.Errorf("the changes start from %q, not %q", h.From, tg.FromSnapshot)
	case !snapshotPattern.MatchString(h.To) || h.To == h.From:
		return 0, fmt.Errorf("invalid snapshot %q", h.To)
	}
	args := []string{"receive"}
	if tg.Rollback {
		if _, err := t.Run(ctx, "zfs", "rollback", "-r", h.Dataset+"@"+h.From); err != nil {
			return 0, err
		}
		args = append(args, "-F")
	}
	cmd := t.Command(ctx, "zfs", append(args, h.Dataset)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return 0, err
	}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("zfs receive: %w", err)
	}
	failed := func(err error) error {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	var n uint64
	for {
		typ, payload, err := f.read()
		if err != nil {
			return n, failed(fmt.Errorf("connection to the DR host: %w", err))
		}
		if typ == frameData {
			if _, err := stdin.Write(payload); err != nil {
				return n, failed(errors.New("zfs receive stopped"))
			}
			n += uint64(len(payload))
			continue
		}
		if typ != frameEnd {
			return n, failed(fmt.Errorf("unexpected frame %q from the DR host", typ))
		}
		if len(payload) > 0 {
			return n, failed(fmt.Errorf("the DR host: %s", payload))
		}
		break
	}
	_ = stdin.Close()
	if err := cmd.Wait(); err != nil {
		return n, fmt.Errorf("zfs receive: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	guid, err := t.guid(ctx, h.Dataset+"@"+h.To)
	if err != nil {
		return n, err
	}
	if guid != h.ToGUID {
		return n, fmt.Errorf("received @%s, but its GUID doesn't match the DR host's", h.To)
	}
	return n, nil
}

// Send connects to the primary and sends each replica's changes.
func (t *Transfer) Send(ctx context.Context, m *clientv1.FailbackSend) (*clientv1.FailbackTransfer, error) {
	for _, s := range m.Datasets {
		if !datasetPattern.MatchString(s.Replica) || !datasetPattern.MatchString(s.Dataset) ||
			!snapshotPattern.MatchString(s.FromSnapshot) || !snapshotPattern.MatchString(s.ToSnapshot) {
			return nil, fmt.Errorf("invalid dataset or snapshot for %q", s.Replica)
		}
	}
	cfg, err := t.tlsConfig(m.GetPeer().GetCertificatePem())
	if err != nil {
		return nil, err
	}
	tc, err := dial(ctx, m.Address, cfg)
	if err != nil {
		return nil, err
	}
	defer tc.Close() //nolint:errcheck // done with the connection
	f := newFramer(tc)
	out := &clientv1.FailbackTransfer{}
	for _, s := range m.Datasets {
		n, err := t.sendOne(ctx, f, s)
		if err != nil {
			return out, fmt.Errorf("%s: %w", s.Dataset, err)
		}
		out.Datasets = append(out.Datasets, &clientv1.DatasetTransfer{Dataset: s.Dataset, Bytes: n})
	}
	if err := f.write(frameQuit, nil); err != nil {
		return out, fmt.Errorf("connection to the primary: %w", err)
	}
	// Wait for the primary to close the connection, so the round ends on
	// both hosts.
	_, _, _ = f.read()
	return out, nil
}

// dial connects to the primary, retrying while it starts listening.
func dial(ctx context.Context, address string, cfg *tls.Config) (*tls.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	var d net.Dialer
	for {
		conn, err := d.DialContext(ctx, "tcp", address)
		if err == nil {
			tc := tls.Client(conn, cfg)
			if err = tc.HandshakeContext(ctx); err == nil {
				return tc, nil
			}
			_ = conn.Close()
			var certErr *pinError
			if errors.As(err, &certErr) {
				return nil, fmt.Errorf("connect to the primary at %s: %w", address, err)
			}
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("connect to the primary at %s: %w", address, err)
		case <-time.After(dialRetry):
		}
	}
}

// sendOne sends one replica's changes and returns the primary's result.
func (t *Transfer) sendOne(ctx context.Context, f *framer, s *clientv1.FailbackSource) (uint64, error) {
	to := s.Replica + "@" + s.ToSnapshot
	guid, err := t.guid(ctx, to)
	if err != nil {
		return 0, err
	}
	h, _ := json.Marshal(header{Dataset: s.Dataset, From: s.FromSnapshot, To: s.ToSnapshot, ToGUID: guid})
	if err := f.write(frameHeader, h); err != nil {
		return 0, fmt.Errorf("connection to the primary: %w", err)
	}
	args := []string{"send", "-L", "-c", "-e"}
	if s.Raw {
		args = []string{"send", "-w"}
	}
	cmd := t.Command(ctx, "zfs", append(args, "-I", s.Replica+"@"+s.FromSnapshot, to)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, err
	}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("zfs send: %w", err)
	}
	var n uint64
	buf := make([]byte, maxFrame)
	for {
		k, rerr := io.ReadFull(stdout, buf)
		if k > 0 {
			if err := f.write(frameData, buf[:k]); err != nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				// The primary may have said why it stopped reading.
				if r, ok := f.result(10 * time.Second); ok && r.Error != "" {
					return n, fmt.Errorf("the primary: %s", r.Error)
				}
				return n, fmt.Errorf("connection to the primary: %w", err)
			}
			n += uint64(k)
		}
		if rerr != nil {
			break
		}
	}
	if err := cmd.Wait(); err != nil {
		msg := fmt.Sprintf("zfs send: %v: %s", err, strings.TrimSpace(stderr.String()))
		_ = f.write(frameEnd, []byte(msg))
		return n, errors.New(msg)
	}
	if err := f.write(frameEnd, nil); err != nil {
		return n, fmt.Errorf("connection to the primary: %w", err)
	}
	r, ok := f.result(idleTimeout)
	switch {
	case !ok:
		return n, errors.New("the primary didn't answer")
	case r.Error != "":
		return n, fmt.Errorf("the primary: %s", r.Error)
	}
	return n, nil
}

func (t *Transfer) guid(ctx context.Context, snapshot string) (uint64, error) {
	out, err := t.Run(ctx, "zfs", "get", "-Hp", "-o", "value", "guid", snapshot)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
}

// tlsConfig returns a TLS configuration with this host's certificate that
// accepts only the peer's exact certificate, as zrepl does.
func (t *Transfer) tlsConfig(peerPEM string) (*tls.Config, error) {
	block, _ := pem.Decode([]byte(peerPEM))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("invalid peer certificate")
	}
	cert, err := t.Cert()
	if err != nil {
		return nil, fmt.Errorf("load this host's certificate: %w", err)
	}
	want := block.Bytes
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		// The peer is verified by pinning its exact certificate below; the
		// certificates are self-signed. VerifyConnection runs on every
		// handshake, and sessions aren't resumed.
		InsecureSkipVerify:     true, //nolint:gosec // pinned in VerifyConnection
		SessionTicketsDisabled: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || !bytes.Equal(cs.PeerCertificates[0].Raw, want) {
				return &pinError{}
			}
			return nil
		},
	}, nil
}

type pinError struct{}

func (*pinError) Error() string { return "the peer's certificate isn't the expected one" }

// freebind lets the listener bind an address that isn't up yet.
func freebind(_, _ string, c syscall.RawConn) error {
	var serr error
	err := c.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), syscall.SOL_IP, syscall.IP_FREEBIND, 1) //nolint:gosec // file descriptors fit in int
	})
	if err != nil {
		return err
	}
	return serr
}

// framer reads and writes frames: a type byte, a 4-byte big-endian length,
// and the payload.
type framer struct {
	conn net.Conn
	r    *bufio.Reader
}

func newFramer(conn net.Conn) *framer {
	return &framer{conn: conn, r: bufio.NewReaderSize(conn, 64<<10)}
}

func (f *framer) write(typ byte, payload []byte) error {
	if len(payload) > maxFrame {
		return errors.New("frame too large")
	}
	var hdr [5]byte
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload))) //nolint:gosec // bounded by maxFrame
	_ = f.conn.SetWriteDeadline(time.Now().Add(idleTimeout))
	if _, err := f.conn.Write(hdr[:]); err != nil {
		return err
	}
	_, err := f.conn.Write(payload)
	return err
}

func (f *framer) read() (byte, []byte, error) {
	return f.readWithin(idleTimeout)
}

func (f *framer) readWithin(d time.Duration) (byte, []byte, error) {
	_ = f.conn.SetReadDeadline(time.Now().Add(d))
	var hdr [5]byte
	if _, err := io.ReadFull(f.r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxFrame {
		return 0, nil, errors.New("frame too large")
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(f.r, payload); err != nil {
		return 0, nil, err
	}
	return hdr[0], payload, nil
}

// result reads the primary's result frame.
func (f *framer) result(d time.Duration) (result, bool) {
	var r result
	typ, payload, err := f.readWithin(d)
	if err != nil || typ != frameResult || json.Unmarshal(payload, &r) != nil {
		return r, false
	}
	return r, true
}
