// Package version exposes build version metadata (version tag, commit SHA, build date),
// injectable at link time via -ldflags or inferred from runtime VCS build info.
package version

import (
	"fmt"
	"runtime/debug"
)

var (
	// Version is the semantic build version or tag (defaults to "dev").
	Version = "dev"
	// Commit is the git commit SHA (defaults to "none").
	Commit = "none"
	// BuildDate is the UTC build timestamp (defaults to "unknown").
	BuildDate = "unknown"
)

func init() {
	if info, ok := debug.ReadBuildInfo(); ok {
		var rev string
		var modified bool
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				modified = (s.Value == "true")
			case "vcs.time":
				if BuildDate == "unknown" {
					BuildDate = s.Value
				}
			}
		}

		if Commit == "none" && rev != "" {
			if len(rev) > 7 {
				Commit = rev[:7]
			} else {
				Commit = rev
			}
			if modified {
				Commit += "-dirty"
			}
		}

		if Version == "dev" && Commit != "none" {
			Version = Commit
		}
	}
}

// String returns a human-readable representation of the build version.
func String() string {
	return fmt.Sprintf("%s (%s, %s)", Version, Commit, BuildDate)
}
