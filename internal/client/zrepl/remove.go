package zrepl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Removal is what removing EZDR from zrepl did.
type Removal struct {
	// Jobs are EZDR's jobs that were removed.
	Jobs []string
	// Released lists the holds and bookmarks released for them.
	Released []string
	// Backup is the main configuration as it was, if it was edited.
	Backup string
}

// EZDRJobs returns the names of the jobs in EZDR's jobs file.
func (a *Applier) EZDRJobs() ([]string, error) {
	cfg, err := readConfigFile(a.Paths.JobsFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, j := range cfg.Jobs {
		names = append(names, j.Name)
	}
	return names, nil
}

// RemoveEZDR removes EZDR from zrepl (docs/design/uninstall.md): the
// include of EZDR's jobs directory in the main configuration (after a
// backup) and the directory, then, once zrepl runs without EZDR's jobs,
// their holds and bookmarks. zrepl keeps its other jobs. Safe to repeat.
func (a *Applier) RemoveEZDR(ctx context.Context) (Removal, error) {
	var r Removal
	jobs, err := a.EZDRJobs()
	if err != nil {
		return r, err
	}
	r.Jobs = jobs
	if b, err := os.ReadFile(a.Paths.MainConfig); err == nil {
		out, changed, err := removeInclude(a.Paths, b)
		if err != nil {
			return r, err
		}
		if changed {
			r.Backup = fmt.Sprintf("%s.ezdr-backup-%s", a.Paths.MainConfig, a.Now().UTC().Format("20060102T150405Z"))
			if err := writeFile(r.Backup, b, 0o644); err != nil {
				return r, err
			}
			if err := writeFile(a.Paths.MainConfig, out, 0o644); err != nil {
				return r, err
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return r, err
	}
	if err := os.RemoveAll(a.Paths.JobsDir); err != nil {
		return r, err
	}
	if a.Version(ctx) == "" {
		return r, nil // zrepl isn't installed: nothing runs EZDR's jobs
	}
	if _, err := a.Run(ctx, "systemctl", "is-active", "--quiet", "zrepl"); err == nil {
		if out, err := a.Run(ctx, "zrepl", "configcheck"); err != nil {
			return r, fmt.Errorf("zrepl rejects its configuration without EZDR's jobs: %w %s", err, strings.TrimSpace(string(out)))
		}
		if _, err := a.Run(ctx, "systemctl", "restart", "zrepl"); err != nil {
			return r, fmt.Errorf("restart zrepl: %w", err)
		}
	}
	r.Released, err = a.ReleaseJobs(ctx, jobs)
	return r, err
}

// removeInclude removes the include entry for EZDR's jobs directory from a
// main configuration, and the include list if it's left empty.
func removeInclude(p Paths, b []byte) ([]byte, bool, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, false, fmt.Errorf("parse %s: %w", p.MainConfig, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, false, fmt.Errorf("%s is not a YAML mapping", p.MainConfig)
	}
	root := doc.Content[0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		list := root.Content[i+1]
		if root.Content[i].Value != "include" || list.Kind != yaml.SequenceNode {
			continue
		}
		n := len(list.Content)
		list.Content = slices.DeleteFunc(list.Content, func(n *yaml.Node) bool {
			return n.Value == p.JobsDir+"/" || n.Value == p.JobsDir
		})
		if len(list.Content) == n {
			return b, false, nil
		}
		if len(list.Content) == 0 {
			root.Content = slices.Delete(root.Content, i, i+2)
		}
		out, err := yaml.Marshal(&doc)
		return out, true, err
	}
	return b, false, nil
}
