package zrepl

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
	planv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/plan/v1"
	"github.com/jlbyh2o/ezdr/internal/plan"
)

// Everything from the portal is validated before it's rendered: the portal
// describes jobs, it can't inject configuration.
var (
	jobNamePattern  = regexp.MustCompile(`^ezdr_[A-Za-z0-9_-]{1,80}$`)
	peerNamePattern = regexp.MustCompile(`^ezdr-[a-z0-9]{1,64}$`)
	datasetPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]*(/[A-Za-z0-9_.:-]+)+$`)
	rootPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]*(/[A-Za-z0-9_.:-]+)*$`)
	prefixPattern   = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,32}$`)
	hostPattern     = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
)

// receiveInherit lists properties a sender must not set on the receiving
// host: a mount point there could be any directory, such as /etc/cron.d.
var receiveInherit = []string{"mountpoint", "sharenfs", "sharesmb"}

// Validate checks every field of the desired zrepl state.
func Validate(z *clientv1.Zrepl) error {
	names := map[string]bool{}
	checkName := func(n string) error {
		if !jobNamePattern.MatchString(n) {
			return fmt.Errorf("invalid job name %q", n)
		}
		if names[n] {
			return fmt.Errorf("duplicate job name %q", n)
		}
		names[n] = true
		return nil
	}
	checkPeer := func(p *clientv1.Peer) error {
		if p == nil || !peerNamePattern.MatchString(p.Name) {
			return errors.New("invalid peer")
		}
		return checkCertificate(p.CertificatePem, p.Name)
	}
	checkInterval := func(sec uint32) error {
		if sec < plan.MinIntervalSeconds || sec > plan.MaxIntervalSeconds {
			return fmt.Errorf("interval %ds out of range", sec)
		}
		return nil
	}
	for _, j := range z.GetSourceJobs() {
		if err := checkName(j.Name); err != nil {
			return err
		}
		if len(j.Datasets) == 0 {
			return fmt.Errorf("job %s has no datasets", j.Name)
		}
		for _, d := range j.Datasets {
			if !validDataset(datasetPattern, d) {
				return fmt.Errorf("job %s: invalid dataset %q", j.Name, d)
			}
		}
		if !prefixPattern.MatchString(j.SnapshotPrefix) {
			return fmt.Errorf("job %s: invalid snapshot prefix", j.Name)
		}
		if err := checkInterval(j.IntervalSeconds); err != nil {
			return fmt.Errorf("job %s: %w", j.Name, err)
		}
		if err := checkAddress(j.ListenAddress, true); err != nil {
			return fmt.Errorf("job %s: %w", j.Name, err)
		}
		if err := checkPeer(j.Peer); err != nil {
			return fmt.Errorf("job %s: %w", j.Name, err)
		}
	}
	for _, j := range z.GetPullJobs() {
		if err := checkName(j.Name); err != nil {
			return err
		}
		if !validDataset(rootPattern, j.ReceiveDataset) {
			return fmt.Errorf("job %s: invalid receive dataset %q", j.Name, j.ReceiveDataset)
		}
		if !prefixPattern.MatchString(j.SnapshotPrefix) {
			return fmt.Errorf("job %s: invalid snapshot prefix", j.Name)
		}
		if err := checkInterval(j.IntervalSeconds); err != nil {
			return fmt.Errorf("job %s: %w", j.Name, err)
		}
		if err := checkAddress(j.Address, false); err != nil {
			return fmt.Errorf("job %s: %w", j.Name, err)
		}
		if err := checkPeer(j.Peer); err != nil {
			return fmt.Errorf("job %s: %w", j.Name, err)
		}
		for _, tiers := range [][]*clientv1.RetentionTier{j.PrimaryRetention, j.DrRetention} {
			if len(tiers) == 0 {
				return fmt.Errorf("job %s: empty retention", j.Name)
			}
			for _, t := range tiers {
				if t.Count == 0 || t.PeriodSeconds == 0 {
					return fmt.Errorf("job %s: invalid retention tier", j.Name)
				}
			}
		}
	}
	return nil
}

// validDataset reports whether name is a dataset name EZDR accepts: the
// pattern matches and no component is "." or "..".
func validDataset(pattern *regexp.Regexp, name string) bool {
	if !pattern.MatchString(name) {
		return false
	}
	for _, c := range strings.Split(name, "/") {
		if c == "." || c == ".." {
			return false
		}
	}
	return true
}

func checkAddress(addr string, allowEmptyHost bool) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid address %q", addr)
	}
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("invalid port in %q", addr)
	}
	if host == "" && allowEmptyHost {
		return nil
	}
	if net.ParseIP(host) == nil && !hostPattern.MatchString(host) {
		return fmt.Errorf("invalid host in %q", addr)
	}
	return nil
}

// duration renders an interval for zrepl, in minutes or hours when exact.
func duration(sec uint32) string {
	switch {
	case sec%3600 == 0:
		return fmt.Sprintf("%dh", sec/3600)
	case sec%60 == 0:
		return fmt.Sprintf("%dm", sec/60)
	default:
		return fmt.Sprintf("%ds", sec)
	}
}

func grid(tiers []*clientv1.RetentionTier) string {
	t := make([]*planv1.RetentionTier, 0, len(tiers))
	for _, x := range tiers {
		t = append(t, &planv1.RetentionTier{Count: x.Count, PeriodSeconds: x.PeriodSeconds, KeepAll: x.KeepAll})
	}
	return plan.Grid(t)
}

type m = map[string]any

// pruneRules keeps the grid for the plan's snapshots, and always keeps
// snapshots without the plan's prefix (such as Proxmox or manual ones).
func pruneRules(prefix string, tiers []*clientv1.RetentionTier, sender bool) []any {
	re := "^" + regexp.QuoteMeta(prefix)
	var rules []any
	if sender {
		// Never prune snapshots the DR host hasn't received yet.
		rules = append(rules, m{"type": "not_replicated"})
	}
	return append(rules,
		m{"type": "grid", "grid": grid(tiers), "regex": re},
		m{"type": "regex", "negate": true, "regex": re},
	)
}

// Render produces EZDR's zrepl job file. The desired state must have been
// validated.
func Render(z *clientv1.Zrepl, p Paths) ([]byte, error) {
	var jobs []any
	for _, j := range z.GetSourceJobs() {
		fs := m{}
		for _, d := range j.Datasets {
			fs[d] = true
		}
		jobs = append(jobs, m{
			"name": j.Name,
			"type": "source",
			"serve": m{
				"type": "tls", "listen": j.ListenAddress, "listen_freebind": j.ListenFreebind,
				"ca": p.PeerFile(j.Peer.Name), "cert": p.CertFile(), "key": p.KeyFile(),
				"client_cns": []string{j.Peer.Name},
			},
			"filesystems": fs,
			"snapshotting": m{
				"type": "periodic", "prefix": j.SnapshotPrefix, "interval": duration(j.IntervalSeconds),
			},
			"send": m{
				"encrypted": j.Encrypted, "compressed": true, "large_blocks": true, "embedded_data": true,
			},
		})
	}
	for _, j := range z.GetPullJobs() {
		jobs = append(jobs, m{
			"name": j.Name,
			"type": "pull",
			"connect": m{
				"type": "tls", "address": j.Address,
				"ca": p.PeerFile(j.Peer.Name), "cert": p.CertFile(), "key": p.KeyFile(),
				"server_cn": j.Peer.Name,
			},
			"root_fs":  j.ReceiveDataset,
			"interval": duration(j.IntervalSeconds),
			"recv": m{
				// zrepl requires this to be set for the placeholder datasets
				// it creates to mirror the source path.
				"placeholder": m{"encryption": "inherit"},
				"properties": m{
					// Nothing on the DR host may change replicas; that would
					// break incremental receives. Failover lifts this.
					"override": m{"readonly": "on"},
					// Replicas mount under the receive dataset, and never
					// where (or as shares) the primary says.
					"inherit": receiveInherit,
				},
			},
			"pruning": m{
				"keep_sender":   pruneRules(j.SnapshotPrefix, j.PrimaryRetention, true),
				"keep_receiver": pruneRules(j.SnapshotPrefix, j.DrRetention, false),
			},
		})
	}
	if jobs == nil {
		jobs = []any{}
	}
	body, err := yaml.Marshal(m{"jobs": jobs})
	if err != nil {
		return nil, err
	}
	header := "# Managed by EZDR. Changes are overwritten; edit the DR plan in the portal instead.\n"
	return append([]byte(header), body...), nil
}

// peerCertificates returns the peer certificate files the jobs need.
func peerCertificates(z *clientv1.Zrepl) map[string]string {
	out := map[string]string{}
	for _, j := range z.GetSourceJobs() {
		out[j.Peer.Name] = strings.TrimSpace(j.Peer.CertificatePem) + "\n"
	}
	for _, j := range z.GetPullJobs() {
		out[j.Peer.Name] = strings.TrimSpace(j.Peer.CertificatePem) + "\n"
	}
	return out
}
