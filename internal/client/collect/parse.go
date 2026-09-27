package collect

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Proxmox's API returns many numbers and booleans as either JSON numbers or
// strings, depending on the endpoint and version. These helpers accept both.

func str(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	default:
		return fmt.Sprint(x)
	}
}

func num(v any) uint64 {
	switch x := v.(type) {
	case float64:
		if x < 0 {
			return 0
		}
		return uint64(x)
	case json.Number:
		n, _ := strconv.ParseFloat(x.String(), 64)
		return num(n)
	case string:
		n, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return 0
		}
		return num(n)
	case bool:
		if x {
			return 1
		}
	}
	return 0
}

func boolean(v any) bool {
	return num(v) != 0 || str(v) == "true"
}

// propertyString parses a Proxmox property string such as
// "local-zfs:vm-201-disk-0,discard=on,size=4G". The first element, if it has
// no "=", is returned as the value of the empty key.
func propertyString(s string) map[string]string {
	m := map[string]string{}
	for i, part := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			if i == 0 {
				m[""] = part
			}
			continue
		}
		m[k] = v
	}
	return m
}

// parseSize parses Proxmox sizes such as "4G", "512M", "528K", or "1T".
func parseSize(s string) uint64 {
	if s == "" {
		return 0
	}
	mult := uint64(1)
	switch s[len(s)-1] {
	case 'K', 'k':
		mult = 1 << 10
	case 'M', 'm':
		mult = 1 << 20
	case 'G', 'g':
		mult = 1 << 30
	case 'T', 't':
		mult = 1 << 40
	}
	if mult != 1 {
		s = s[:len(s)-1]
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0
	}
	return uint64(f * float64(mult))
}

// parseMemoryMB parses guest memory, which may be "1024" or "current=1024".
func parseMemoryMB(v any) uint64 {
	s := str(v)
	if s == "" {
		return 0
	}
	if m := propertyString(s); m["current"] != "" {
		s = m["current"]
	} else if m[""] != "" {
		s = m[""]
	}
	return num(s)
}

// splitTags splits Proxmox guest tags, which may be separated by ';', ',', or
// spaces.
func splitTags(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ';' || r == ',' || r == ' ' })
}
