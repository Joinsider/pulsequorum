// Package version exposes build metadata injected via -ldflags.
package version

// Set at build time, e.g.
// -X github.com/joinsider/uptimecluster/internal/version.Version=v0.1.0
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)
