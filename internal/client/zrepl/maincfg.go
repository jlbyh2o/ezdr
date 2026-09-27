package zrepl

import (
	"errors"
	"fmt"
	"os"
	"time"

	"go.yaml.in/yaml/v3"
)

const defaultMainConfig = `# Created by EZDR. Other jobs may be added here; EZDR's jobs are in the
# included directory.
global:
  logging:
    - type: syslog
      format: human
      level: info
include:
  - %s/
`

// ensureInclude makes the main zrepl configuration include EZDR's jobs
// directory, creating the file if needed. An existing file is edited
// structurally (keeping its jobs and comments) after a timestamped backup.
// It returns the backup path ("" if none was made) and whether it changed
// anything.
func ensureInclude(p Paths, now time.Time) (backup string, changed bool, err error) {
	// zrepl 0.7.0 resolves relative include paths against its working
	// directory, so the path is always absolute.
	include := p.JobsDir + "/"
	b, err := os.ReadFile(p.MainConfig)
	if errors.Is(err, os.ErrNotExist) {
		return "", true, writeFile(p.MainConfig, fmt.Appendf(nil, defaultMainConfig, p.JobsDir), 0o644)
	}
	if err != nil {
		return "", false, err
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return "", false, fmt.Errorf("parse %s: %w", p.MainConfig, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return "", false, fmt.Errorf("%s is not a YAML mapping", p.MainConfig)
	}
	root := doc.Content[0]
	var list *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "include" {
			list = root.Content[i+1]
		}
	}
	switch {
	case list == nil:
		list = &yaml.Node{Kind: yaml.SequenceNode}
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "include"}, list)
	case list.Kind != yaml.SequenceNode:
		return "", false, fmt.Errorf("%s: include is not a list", p.MainConfig)
	}
	for _, n := range list.Content {
		if n.Value == include || n.Value == p.JobsDir {
			return "", false, nil
		}
	}
	list.Content = append(list.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: include})

	out, err := yaml.Marshal(&doc)
	if err != nil {
		return "", false, err
	}
	backup = fmt.Sprintf("%s.ezdr-backup-%s", p.MainConfig, now.UTC().Format("20060102T150405Z"))
	if err := writeFile(backup, b, 0o644); err != nil {
		return "", false, err
	}
	return backup, true, writeFile(p.MainConfig, out, 0o644)
}
