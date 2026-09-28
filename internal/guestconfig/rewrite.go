// Package guestconfig rewrites protected guests' Proxmox configurations for
// the DR host: test failover now, failover in phase 6. See
// docs/design/test-failover.md, section 3.
package guestconfig

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Mapping says how to rewrite one guest's configuration.
type Mapping struct {
	// Type is "qemu" or "lxc".
	Type string
	// Volumes maps each replicated disk's volume ID in the original
	// configuration (such as "local-zfs:vm-201-disk-0") to its volume ID on
	// the DR host.
	Volumes map[string]string
	// Bridge replaces every NIC's bridge.
	Bridge string
	// Storages are the DR host's storage IDs; media on others is removed.
	Storages []string
	// Name, when set, replaces a VM's name. A container's hostname is kept:
	// it's the guest's own hostname.
	Name string
	// Tag is added to the guest's tags.
	Tag string
	// Note is prepended to the description.
	Note string
	// VMGenID replaces a VM's generation ID (a UUID).
	VMGenID string
}

// Result is a rewritten configuration.
type Result struct {
	Config string
	// CloudInit is the key of a cloud-init drive that was removed and must be
	// recreated on the DR host (such as "ide2"), if any.
	CloudInit string
	// Removed describes settings dropped because they can't work on the DR
	// host, such as passthrough devices.
	Removed []string
}

var (
	vmDiskKey = regexp.MustCompile(`^((ide|sata|scsi|virtio)\d+|efidisk0|tpmstate0)$`)
	ctDiskKey = regexp.MustCompile(`^(rootfs|mp\d+)$`)
	nicKey    = regexp.MustCompile(`^net\d+$`)
	// Settings that can't be carried to another host.
	dropKey = regexp.MustCompile(`^(hostpci\d+|usb\d+|dev\d+|unused\d+|lock|parent|hookscript)$`)
	uuidPat = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// Rewrite applies m to a configuration (without snapshot sections).
func Rewrite(conf string, m Mapping) (Result, error) {
	if m.Type != "qemu" && m.Type != "lxc" {
		return Result{}, fmt.Errorf("unknown guest type %q", m.Type)
	}
	if m.Type == "qemu" && !uuidPat.MatchString(m.VMGenID) {
		return Result{}, fmt.Errorf("invalid VM generation ID %q", m.VMGenID)
	}
	var res Result
	var desc, out []string
	used := map[string]bool{}
	seen := map[string]bool{}
	for line := range strings.SplitSeq(conf, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if d, ok := strings.CutPrefix(line, "#"); ok {
			desc = append(desc, d)
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return Result{}, fmt.Errorf("unreadable line %q", line)
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		seen[key] = true
		switch {
		case dropKey.MatchString(key):
			if !strings.HasPrefix(key, "unused") && key != "lock" && key != "parent" {
				res.Removed = append(res.Removed, key+": "+value)
			}
			continue
		case key == "onboot", key == "vmgenid" && m.Type == "qemu":
			continue // set below
		case key == "name" && m.Type == "qemu" && m.Name != "":
			value = m.Name
		case key == "tags":
			value = addTag(value, m.Tag)
		case nicKey.MatchString(key):
			value = setOption(value, "bridge", m.Bridge)
		case (m.Type == "qemu" && vmDiskKey.MatchString(key)) || (m.Type == "lxc" && ctDiskKey.MatchString(key)):
			v, drop, err := rewriteDisk(key, value, m, &res)
			if err != nil {
				return Result{}, err
			}
			if drop {
				continue
			}
			vol, _, _ := strings.Cut(value, ",")
			used[vol] = true
			value = v
		}
		out = append(out, key+": "+value)
	}
	for vol := range m.Volumes {
		if !used[vol] {
			return Result{}, fmt.Errorf("volume %s isn't in the configuration", vol)
		}
	}
	out = append(out, "onboot: 0")
	if m.Type == "qemu" {
		out = append(out, "vmgenid: "+m.VMGenID)
	}
	if !seen["tags"] && m.Tag != "" {
		out = append(out, "tags: "+m.Tag)
	}
	slices.Sort(out)

	var b strings.Builder
	if m.Note != "" {
		b.WriteString("#" + m.Note + "\n")
		if len(desc) > 0 {
			b.WriteString("#\n")
		}
	}
	for _, d := range desc {
		b.WriteString("#" + d + "\n")
	}
	for _, l := range out {
		b.WriteString(l + "\n")
	}
	res.Config = b.String()
	return res, nil
}

// rewriteDisk maps one disk or mount point. It reports whether to drop the
// line (a cloud-init drive to recreate, or unusable media).
func rewriteDisk(key, value string, m Mapping, res *Result) (string, bool, error) {
	vol, opts, _ := strings.Cut(value, ",")
	cdrom := strings.Contains(","+opts+",", ",media=cdrom,")
	switch {
	case m.Volumes[vol] != "":
		return m.Volumes[vol] + optsSuffix(opts), false, nil
	case strings.Contains(vol, "cloudinit") && cdrom:
		res.CloudInit = key
		return "", true, nil
	case cdrom:
		// ISO media: keep it if the DR host has that storage.
		storage, _, isVolume := strings.Cut(vol, ":")
		if vol == "none" || vol == "cdrom" || (isVolume && slices.Contains(m.Storages, storage)) {
			return value, false, nil
		}
		res.Removed = append(res.Removed, key+": "+value)
		return "none,media=cdrom", false, nil
	case strings.HasPrefix(vol, "/"):
		// A container bind mount or a VM's host device: the path may not
		// exist on the DR host.
		res.Removed = append(res.Removed, key+": "+value)
		return "", true, nil
	}
	return "", false, fmt.Errorf("%s: volume %s wasn't replicated", key, vol)
}

func optsSuffix(opts string) string {
	if opts == "" {
		return ""
	}
	return "," + opts
}

// setOption sets key=value in a comma-separated option list.
func setOption(list, key, value string) string {
	parts := strings.Split(list, ",")
	for i, p := range parts {
		if k, _, ok := strings.Cut(p, "="); ok && k == key {
			parts[i] = key + "=" + value
			return strings.Join(parts, ",")
		}
	}
	return list + "," + key + "=" + value
}

// addTag adds tag to Proxmox's semicolon-separated tag list.
func addTag(tags, tag string) string {
	if tag == "" {
		return tags
	}
	list := strings.FieldsFunc(tags, func(r rune) bool { return r == ';' || r == ',' || r == ' ' })
	if !slices.Contains(list, tag) {
		list = append(list, tag)
	}
	return strings.Join(list, ";")
}
