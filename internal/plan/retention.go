// Package plan implements DR plan defaults, suggestions, and validation. See
// docs/design/dr-plans.md.
package plan

import (
	"fmt"
	"strings"

	planv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/plan/v1"
)

// Defaults for new plans.
const (
	DefaultIntervalSeconds = 15 * 60
	MinIntervalSeconds     = 60
	MaxIntervalSeconds     = 24 * 3600
	DefaultSnapshotPrefix  = "ezdr_"
)

const (
	hour = 3600
	day  = 24 * hour
)

func tier(count, period uint32, keepAll bool) *planv1.RetentionTier {
	return &planv1.RetentionTier{Count: count, PeriodSeconds: period, KeepAll: keepAll}
}

// RetentionPresets are the DR host retention presets offered in the editor.
var RetentionPresets = map[string][]*planv1.RetentionTier{
	"balanced":        {tier(1, day, true), tier(14, day, false), tier(8, 7*day, false)},
	"hourly-daily":    {tier(1, hour, true), tier(24, hour, false), tier(14, day, false)},
	"minimal":         {tier(1, day, true), tier(7, day, false)},
	"extended":        {tier(1, day, true), tier(14, day, false), tier(8, 7*day, false), tier(12, 30*day, false)},
	"primary-default": {tier(1, day, true)},
}

// DefaultDRRetention returns the default DR host retention (balanced).
func DefaultDRRetention() []*planv1.RetentionTier { return clone(RetentionPresets["balanced"]) }

// DefaultPrimaryRetention returns the default primary retention (24 hours).
func DefaultPrimaryRetention() []*planv1.RetentionTier {
	return clone(RetentionPresets["primary-default"])
}

func clone(tiers []*planv1.RetentionTier) []*planv1.RetentionTier {
	out := make([]*planv1.RetentionTier, len(tiers))
	for i, t := range tiers {
		out[i] = tier(t.Count, t.PeriodSeconds, t.KeepAll)
	}
	return out
}

// Grid renders tiers in zrepl's grid syntax, such as
// "1x1h(keep=all) | 24x1h | 14x1d".
func Grid(tiers []*planv1.RetentionTier) string {
	parts := make([]string, 0, len(tiers))
	for _, t := range tiers {
		p := fmt.Sprintf("%dx%s", t.Count, Duration(t.PeriodSeconds))
		if t.KeepAll {
			p += "(keep=all)"
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, " | ")
}

// Duration renders seconds in the largest whole unit zrepl understands.
func Duration(seconds uint32) string {
	switch {
	case seconds != 0 && seconds%day == 0:
		return fmt.Sprintf("%dd", seconds/day)
	case seconds != 0 && seconds%hour == 0:
		return fmt.Sprintf("%dh", seconds/hour)
	case seconds != 0 && seconds%60 == 0:
		return fmt.Sprintf("%dm", seconds/60)
	default:
		return fmt.Sprintf("%ds", seconds)
	}
}

// Span is the total time a list of tiers covers, in seconds.
func Span(tiers []*planv1.RetentionTier) uint64 {
	var total uint64
	for _, t := range tiers {
		total += uint64(t.Count) * uint64(t.PeriodSeconds)
	}
	return total
}
