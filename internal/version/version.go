// Package version is the single source of build identity. Version and Commit
// are stamped in at release time via -ldflags; dev builds fall back to module
// build info so the values stay useful without a release.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"sync"
)

// Version is the build version, overridden via -ldflags at release time.
var Version = "dev"

// Commit is the VCS revision, overridden via -ldflags at release time. When
// left at the default it is filled from module build info if available.
var Commit = ""

var commitOnce sync.Once

// commit returns the resolved VCS revision, preferring the ldflags value and
// falling back to the embedded build info for dev builds.
func commit() string {
	commitOnce.Do(func() {
		if Commit != "" {
			return
		}
		Commit = "unknown"
		info, ok := debug.ReadBuildInfo()
		if !ok {
			return
		}
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && s.Value != "" {
				Commit = s.Value
				return
			}
		}
	})
	return Commit
}

// String returns a human-readable build identity: version, commit, and
// GOOS/GOARCH.
func String() string {
	return fmt.Sprintf("open-artifact %s (commit %s, %s/%s)",
		Version, commit(), runtime.GOOS, runtime.GOARCH)
}
