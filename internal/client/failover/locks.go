// Package failover runs failovers: on the primary it stops and locks the
// failed-over guests, and on the DR host it brings up guests from their
// replicas. See docs/design/failover.md.
package failover

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jlbyh2o/ezdr/internal/client/guests"
)

// LockTag marks guests on the primary that were failed over, and OnbootTag
// those whose onboot setting unlocking restores.
const (
	LockTag   = "ezdr-failed-over"
	OnbootTag = "ezdr-onboot"
)

// Runner performs failover steps. Tests replace its fields.
type Runner struct {
	// PVE is Proxmox's cluster file system root.
	PVE    string
	Guests guests.Paths
	Run    func(ctx context.Context, name string, args ...string) ([]byte, error)
	Node   func() (string, error)
}

// NewRunner returns a Runner for the real host.
func NewRunner() *Runner {
	return &Runner{PVE: "/etc/pve", Guests: guests.DefaultPaths, Run: runCommand, Node: nodeName}
}

func nodeName() (string, error) {
	h, err := os.Hostname()
	short, _, _ := strings.Cut(h, ".")
	return short, err
}

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // fixed commands from this package
	cmd.Dir = "/"
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
		}
		return out, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return out, nil
}

// guest returns a guest's type ("qemu" or "lxc") and configuration, or
// os.ErrNotExist.
func (r *Runner) guest(vmid uint32) (string, string, error) {
	for _, typ := range []string{"qemu", "lxc"} {
		b, err := os.ReadFile(r.configPath(typ, vmid))
		if err == nil {
			return typ, string(b), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", "", err
		}
	}
	return "", "", os.ErrNotExist
}

func (r *Runner) configPath(typ string, vmid uint32) string {
	dir := "qemu-server"
	if typ == "lxc" {
		dir = "lxc"
	}
	return filepath.Join(r.PVE, dir, strconv.FormatUint(uint64(vmid), 10)+".conf")
}

func tool(typ string) string {
	if typ == "lxc" {
		return "pct"
	}
	return "qm"
}

// value returns a top-level setting of a configuration (ignoring snapshot
// sections).
func value(conf, key string) string {
	for line := range strings.SplitSeq(conf, "\n") {
		if strings.HasPrefix(line, "[") {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func tags(conf string) []string {
	return strings.FieldsFunc(value(conf, "tags"), func(r rune) bool { return r == ';' || r == ',' || r == ' ' })
}

// Locked reports whether a guest carries EZDR's failover lock.
func Locked(conf string) bool {
	return value(conf, "lock") == "migrate" && slices.Contains(tags(conf), LockTag)
}

func (r *Runner) running(ctx context.Context, typ string, vmid uint32) (bool, error) {
	out, err := r.Run(ctx, tool(typ), "status", strconv.FormatUint(uint64(vmid), 10))
	if err != nil {
		return false, err
	}
	return strings.Contains(string(out), "status: running"), nil
}

// StopAndLock shuts down the guests in the given order (forcing them off
// after the timeout) and locks them so they can't start: onboot off, the
// ezdr-failed-over tag, and Proxmox's migrate lock. Guests that are already
// locked are left alone; guests that don't exist are skipped. It returns
// what it did.
func (r *Runner) StopAndLock(ctx context.Context, vmids []uint32, timeoutSeconds uint32) ([]string, error) {
	var events []string
	var errs []error
	for _, vmid := range vmids {
		ev, err := r.stopAndLock(ctx, vmid, timeoutSeconds)
		events = append(events, ev...)
		if err != nil {
			errs = append(errs, fmt.Errorf("guest %d: %w", vmid, err))
		}
	}
	return events, errors.Join(errs...)
}

func (r *Runner) stopAndLock(ctx context.Context, vmid, timeoutSeconds uint32) ([]string, error) {
	typ, conf, err := r.guest(vmid)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if Locked(conf) {
		return nil, nil
	}
	id := strconv.FormatUint(uint64(vmid), 10)
	var events []string
	running, err := r.running(ctx, typ, vmid)
	if err != nil {
		return nil, err
	}
	if running {
		args := []string{"shutdown", id, "--timeout", strconv.FormatUint(uint64(timeoutSeconds), 10), "--forceStop", "1"}
		if typ == "qemu" {
			args = append(args, "--skiplock", "1")
		}
		if _, err := r.Run(ctx, tool(typ), args...); err != nil {
			return nil, err
		}
		events = append(events, fmt.Sprintf("stopped guest %d", vmid))
	}
	t := tags(conf)
	if !slices.Contains(t, LockTag) {
		t = append(t, LockTag)
	}
	// Remember onboot, so unlocking can restore it.
	if value(conf, "onboot") == "1" && !slices.Contains(t, OnbootTag) {
		t = append(t, OnbootTag)
	}
	set := []string{"set", id, "--onboot", "0", "--tags", strings.Join(t, ";")}
	if typ == "qemu" {
		set = append(set, "--skiplock", "1")
	}
	if _, err := r.Run(ctx, tool(typ), set...); err != nil {
		return events, err
	}
	lock := []string{"set", id, "--lock", "migrate"}
	if typ == "qemu" {
		lock = append(lock, "--skiplock", "1")
	}
	if _, err := r.Run(ctx, tool(typ), lock...); err != nil {
		return events, err
	}
	return append(events, fmt.Sprintf("locked guest %d", vmid)), nil
}

// Unlock reverses StopAndLock for the guests, in the given order: it removes
// EZDR's lock and tags and restores onboot, and starts them if start is set.
// Guests EZDR didn't lock are left alone.
func (r *Runner) Unlock(ctx context.Context, vmids []uint32, start bool) ([]string, error) {
	var events []string
	var errs []error
	for _, vmid := range vmids {
		typ, conf, err := r.guest(vmid)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		id := strconv.FormatUint(uint64(vmid), 10)
		if Locked(conf) {
			t := slices.DeleteFunc(tags(conf), func(s string) bool { return s == LockTag || s == OnbootTag })
			onboot := "0"
			if slices.Contains(tags(conf), OnbootTag) {
				onboot = "1"
			}
			set := []string{"set", id, "--onboot", onboot}
			if len(t) > 0 {
				set = append(set, "--tags", strings.Join(t, ";"))
			} else {
				set = append(set, "--delete", "tags")
			}
			if typ == "qemu" {
				set = append(set, "--skiplock", "1")
			}
			if _, err := r.Run(ctx, tool(typ), "unlock", id); err != nil {
				errs = append(errs, err)
				continue
			}
			if _, err := r.Run(ctx, tool(typ), set...); err != nil {
				errs = append(errs, err)
				continue
			}
			events = append(events, fmt.Sprintf("unlocked guest %d", vmid))
		} else if slices.Contains(tags(conf), LockTag) || value(conf, "lock") != "" {
			continue // someone else's lock: leave it
		}
		if start {
			if running, _ := r.running(ctx, typ, vmid); !running {
				if _, err := r.Run(ctx, tool(typ), "start", id); err != nil {
					errs = append(errs, err)
					continue
				}
				events = append(events, fmt.Sprintf("started guest %d", vmid))
			}
		}
	}
	return events, errors.Join(errs...)
}
