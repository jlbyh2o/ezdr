package zrepl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
)

// Applier applies zrepl desired state to the host.
type Applier struct {
	Paths Paths
	// Run executes a command; tests replace it.
	Run func(ctx context.Context, name string, args ...string) ([]byte, error)
	Now func() time.Time
}

// NewApplier returns an Applier for the real host.
func NewApplier() *Applier {
	return &Applier{Paths: DefaultPaths, Run: runCommand, Now: time.Now}
}

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // fixed commands from this package
	cmd.Dir = "/"
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return out, fmt.Errorf("%w: %s", err, msg)
		}
		return out, err
	}
	return out, nil
}

// Apply makes the host's zrepl configuration match z. With no jobs, zrepl
// isn't installed, and EZDR's jobs are removed if they exist. Replicas and
// snapshots are never touched here.
func (a *Applier) Apply(ctx context.Context, z *clientv1.Zrepl) error {
	hasJobs := len(z.GetSourceJobs())+len(z.GetPullJobs()) > 0
	if !hasJobs {
		if _, err := os.Stat(a.Paths.JobsFile()); errors.Is(err, os.ErrNotExist) {
			return nil
		}
	}
	if err := Validate(z); err != nil {
		return fmt.Errorf("invalid configuration from portal: %w", err)
	}
	if hasJobs && a.Version(ctx) == "" {
		if err := a.Install(ctx); err != nil {
			return fmt.Errorf("install zrepl: %w", err)
		}
	}

	if err := os.MkdirAll(a.Paths.JobsDir, 0o750); err != nil {
		return err
	}
	if err := os.MkdirAll(a.Paths.PeersDir, 0o750); err != nil {
		return err
	}
	certsChanged := false
	for name, pemText := range peerCertificates(z) {
		old, _ := os.ReadFile(a.Paths.PeerFile(name))
		if string(old) != pemText {
			if err := writeFile(a.Paths.PeerFile(name), []byte(pemText), 0o644); err != nil {
				return err
			}
			certsChanged = true
		}
	}

	// zrepl requires each pull job's receive dataset (root_fs) to exist.
	for _, j := range z.GetPullJobs() {
		if err := a.ensureDataset(ctx, j.ReceiveDataset); err != nil {
			return err
		}
	}

	rendered, err := Render(z, a.Paths)
	if err != nil {
		return err
	}
	old, readErr := os.ReadFile(a.Paths.JobsFile())
	hadOld := readErr == nil

	// The jobs file must exist before the main configuration includes it.
	if !hadOld {
		if err := writeFile(a.Paths.JobsFile(), []byte("jobs: []\n"), 0o644); err != nil {
			return err
		}
	}
	backup, includeChanged, err := ensureInclude(a.Paths, a.Now())
	if err != nil {
		return err
	}

	if hadOld && bytes.Equal(old, rendered) && !includeChanged && !certsChanged {
		return a.ensureRunning(ctx)
	}
	restore := func() {
		if hadOld {
			_ = writeFile(a.Paths.JobsFile(), old, 0o644)
		} else {
			_ = os.Remove(a.Paths.JobsFile())
		}
		if backup != "" {
			if b, err := os.ReadFile(backup); err == nil { //nolint:gosec // our own backup file
				_ = writeFile(a.Paths.MainConfig, b, 0o644)
			}
		}
	}
	if err := writeFile(a.Paths.JobsFile(), rendered, 0o644); err != nil {
		restore()
		return err
	}
	if out, err := a.Run(ctx, "zrepl", "configcheck"); err != nil {
		restore()
		return fmt.Errorf("zrepl rejected the configuration (previous configuration kept): %w %s", err, strings.TrimSpace(string(out)))
	}
	if _, err := a.Run(ctx, "systemctl", "restart", "zrepl"); err != nil {
		return fmt.Errorf("restart zrepl: %w", err)
	}
	return a.ensureRunning(ctx)
}

func (a *Applier) ensureRunning(ctx context.Context) error {
	if _, err := a.Run(ctx, "systemctl", "is-active", "--quiet", "zrepl"); err == nil {
		return nil
	}
	if _, err := a.Run(ctx, "systemctl", "start", "zrepl"); err != nil {
		return fmt.Errorf("start zrepl: %w", err)
	}
	return nil
}

// ensureDataset creates a receive dataset (and its parents) if it doesn't
// exist. It isn't mounted: it only holds replicas.
func (a *Applier) ensureDataset(ctx context.Context, name string) error {
	if _, err := a.Run(ctx, "zfs", "list", "-H", "-o", "name", name); err == nil {
		return nil
	}
	if _, err := a.Run(ctx, "zfs", "create", "-p", "-o", "canmount=off", name); err != nil {
		return fmt.Errorf("create receive dataset %s: %w", name, err)
	}
	return nil
}
