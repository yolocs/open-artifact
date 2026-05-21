package version

import (
	"runtime"
	"strings"
	"testing"
)

func TestString(t *testing.T) {
	t.Parallel()

	got := String()
	for _, want := range []string{"open-artifact", Version, runtime.GOOS, runtime.GOARCH} {
		if !strings.Contains(got, want) {
			t.Errorf("String() = %q, missing %q", got, want)
		}
	}
}

func TestCommitFallback(t *testing.T) {
	t.Parallel()

	// In a test binary the ldflags value is empty, so commit() must fall back
	// to build info (a revision) or the "unknown" sentinel — never empty.
	if got := commit(); got == "" {
		t.Error("commit() returned empty string; want a revision or 'unknown'")
	}
}
