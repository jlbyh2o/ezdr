// Package version holds build information set at link time.
package version

// These values are overridden at build time with -ldflags "-X ...".
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// String returns a human-readable version string.
func String() string {
	return Version + " (commit " + Commit + ", built " + Date + ")"
}
