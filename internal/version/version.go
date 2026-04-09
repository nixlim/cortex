// Package version exposes the Cortex build version.
package version

// Version is the semantic version of the Cortex binary.
// Phase 1 ships under 0.1.0. Release tooling may override this at link time
// with -ldflags "-X github.com/nixlim/cortex/internal/version.Version=...".
var Version = "0.1.0"

// String returns the version as a plain semver string.
func String() string { return Version }
