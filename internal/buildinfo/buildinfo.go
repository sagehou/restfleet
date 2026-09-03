// Package buildinfo exposes build metadata shared by RestFleet binaries.
package buildinfo

import "fmt"

var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// String returns a human-readable, non-sensitive build identifier.
func String() string {
	return fmt.Sprintf("%s (commit=%s, built=%s)", Version, Commit, Date)
}
