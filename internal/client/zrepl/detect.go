package zrepl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	inventoryv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/inventory/v1"
)

// Detect describes the host's zrepl installation and every configured job,
// including jobs EZDR doesn't manage, for the inventory.
func (a *Applier) Detect(ctx context.Context) *inventoryv1.Zrepl {
	z := &inventoryv1.Zrepl{Version: a.Version(ctx)}
	if z.Version != "" {
		_, err := a.Run(ctx, "systemctl", "is-active", "--quiet", "zrepl")
		z.Running = err == nil
	}
	jobs, err := ReadJobs(a.Paths)
	z.Jobs = jobs
	if err != nil {
		z.ConfigError = err.Error()
	}
	return z
}

// configFile is the part of a zrepl configuration file that detection reads.
type configFile struct {
	Include []string    `yaml:"include"`
	Jobs    []jobConfig `yaml:"jobs"`
}

type jobConfig struct {
	Name         string     `yaml:"name"`
	Type         string     `yaml:"type"`
	Serve        *transport `yaml:"serve"`
	Connect      *transport `yaml:"connect"`
	Filesystems  yaml.Node  `yaml:"filesystems"`
	Snapshotting struct {
		Type     string `yaml:"type"`
		Prefix   string `yaml:"prefix"`
		Interval string `yaml:"interval"`
	} `yaml:"snapshotting"`
	Interval string `yaml:"interval"`
	RootFS   string `yaml:"root_fs"`
	Send     struct {
		Encrypted    bool `yaml:"encrypted"`
		Raw          bool `yaml:"raw"`
		Compressed   bool `yaml:"compressed"`
		LargeBlocks  bool `yaml:"large_blocks"`
		EmbeddedData bool `yaml:"embedded_data"`
	} `yaml:"send"`
	Pruning struct {
		KeepSender   []pruneRule `yaml:"keep_sender"`
		KeepReceiver []pruneRule `yaml:"keep_receiver"`
	} `yaml:"pruning"`
}

type transport struct {
	Type           string   `yaml:"type"`
	Listen         string   `yaml:"listen"`
	ListenFreebind bool     `yaml:"listen_freebind"`
	Address        string   `yaml:"address"`
	ServerCN       string   `yaml:"server_cn"`
	ClientCNs      []string `yaml:"client_cns"`
}

type pruneRule struct {
	Type   string `yaml:"type"`
	Grid   string `yaml:"grid"`
	Regex  string `yaml:"regex"`
	Negate bool   `yaml:"negate"`
	Count  uint32 `yaml:"count"`
}

// ReadJobs parses the main zrepl configuration and the files it includes.
// A missing main configuration means no jobs. Files that can't be read are
// skipped and reported in the error; jobs from the others are still
// returned.
func ReadJobs(p Paths) ([]*inventoryv1.ZreplJob, error) {
	main, err := readConfigFile(p.MainConfig)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var errs []error
	jobs := summarizeJobs(main.Jobs, p.MainConfig, false)
	for _, inc := range main.Include {
		files, err := includedFiles(inc)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, f := range files {
			cfg, err := readConfigFile(f)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			jobs = append(jobs, summarizeJobs(cfg.Jobs, f, f == p.JobsFile())...)
		}
	}
	return jobs, errors.Join(errs...)
}

func readConfigFile(path string) (*configFile, error) {
	b, err := os.ReadFile(path) //nolint:gosec // zrepl configuration paths
	if err != nil {
		return nil, err
	}
	var cfg configFile
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &cfg, nil
}

// includedFiles lists the files an include entry refers to: the file itself,
// or a directory's .yml and .yaml files in name order. zrepl resolves
// relative paths against its working directory, which is / under systemd.
func includedFiles(inc string) ([]string, error) {
	if !filepath.IsAbs(inc) {
		inc = filepath.Join("/", inc)
	}
	st, err := os.Stat(inc)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return []string{inc}, nil
	}
	entries, err := os.ReadDir(inc)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if ext := filepath.Ext(e.Name()); !e.IsDir() && (ext == ".yml" || ext == ".yaml") {
			files = append(files, filepath.Join(inc, e.Name()))
		}
	}
	slices.Sort(files)
	return files, nil
}

func summarizeJobs(cfgs []jobConfig, file string, managed bool) []*inventoryv1.ZreplJob {
	jobs := make([]*inventoryv1.ZreplJob, 0, len(cfgs))
	for _, c := range cfgs {
		j := &inventoryv1.ZreplJob{
			Name: c.Name, Type: c.Type, File: file, Managed: managed,
			SnapshottingType: c.Snapshotting.Type, SnapshotPrefix: c.Snapshotting.Prefix,
			SnapshotIntervalSeconds: seconds(c.Snapshotting.Interval),
			IntervalSeconds:         seconds(c.Interval),
			RootFs:                  c.RootFS,
			Send: &inventoryv1.ZreplSend{
				Encrypted: c.Send.Encrypted, Raw: c.Send.Raw, Compressed: c.Send.Compressed,
				LargeBlocks: c.Send.LargeBlocks, EmbeddedData: c.Send.EmbeddedData,
			},
			KeepSender:   detectedRules(c.Pruning.KeepSender),
			KeepReceiver: detectedRules(c.Pruning.KeepReceiver),
		}
		if t := c.Serve; t != nil {
			j.Transport, j.ListenAddress, j.ListenFreebind, j.ClientCns = t.Type, t.Listen, t.ListenFreebind, t.ClientCNs
		}
		if t := c.Connect; t != nil {
			j.Transport, j.ConnectAddress, j.ServerCn = t.Type, t.Address, t.ServerCN
		}
		// The filter is a mapping; keep its order for display.
		if n := c.Filesystems; n.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(n.Content); i += 2 {
				include, _ := strconv.ParseBool(n.Content[i+1].Value)
				j.Filesystems = append(j.Filesystems, &inventoryv1.ZreplFilter{Pattern: n.Content[i].Value, Include: include})
			}
		}
		jobs = append(jobs, j)
	}
	return jobs
}

func detectedRules(rules []pruneRule) []*inventoryv1.ZreplPruneRule {
	out := make([]*inventoryv1.ZreplPruneRule, 0, len(rules))
	for _, r := range rules {
		out = append(out, &inventoryv1.ZreplPruneRule{Type: r.Type, Grid: r.Grid, Regex: r.Regex, Negate: r.Negate, Count: r.Count})
	}
	return out
}

// seconds parses a zrepl duration such as "5m" or "1d"; anything else, such
// as "manual", is 0.
func seconds(s string) uint32 {
	s = strings.TrimSpace(s)
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.ParseUint(days, 10, 16)
		if err != nil {
			return 0
		}
		return uint32(n) * 86400 //nolint:gosec // bounded by the 16-bit parse
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 || d.Seconds() > float64(^uint32(0)) {
		return 0
	}
	return uint32(d.Seconds())
}
