//go:build integration

package debian_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func requireApt(t *testing.T) string {
	t.Helper()
	aptGet, err := exec.LookPath("apt-get")
	if err != nil {
		t.Skipf("apt-get is not available: %v", err)
	}
	if _, err := exec.LookPath("dpkg"); err != nil {
		t.Skipf("dpkg is not available: %v", err)
	}
	return aptGet
}

func dpkgArch(t *testing.T) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "dpkg", "--print-architecture").Output()
	if err != nil {
		t.Skipf("dpkg --print-architecture failed: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// aptEnv runs apt-get with every directory redirected into a temp root so it
// works without root privileges.
type aptEnv struct {
	opts    []string
	timeout time.Duration
}

// newAptEnv writes an isolated apt configuration pointing at repoURL for the
// given suite/arch. When trusted is true the sources entry carries
// `[trusted=yes]` so apt skips signature verification (used by the hermetic
// test against an unsigned fake repo); when false, apt verifies the repo
// signature against the host's system keyring (used by the live-upstream test
// against a real, signed Ubuntu mirror), so trustedparts is left at its default.
func newAptEnv(t *testing.T, root, repoURL, arch string, trusted bool, suite string) *aptEnv {
	t.Helper()
	listsDir := filepath.Join(root, "lists")
	archivesDir := filepath.Join(root, "archives")
	etcDir := filepath.Join(root, "etc")
	for _, d := range []string{
		filepath.Join(listsDir, "partial"),
		filepath.Join(archivesDir, "partial"),
		etcDir,
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	options := "arch=" + arch
	if trusted {
		options = "trusted=yes " + options
	}
	sourcesFile := filepath.Join(etcDir, "sources.list")
	if err := os.WriteFile(sourcesFile,
		[]byte(fmt.Sprintf("deb [%s] %s %s main\n", options, repoURL, suite)), 0o644); err != nil {
		t.Fatalf("write sources.list: %v", err)
	}
	statusFile := filepath.Join(etcDir, "status")
	if err := os.WriteFile(statusFile, nil, 0o644); err != nil {
		t.Fatalf("write status: %v", err)
	}

	opts := []string{
		"-o", "Dir::Etc::sourcelist=" + sourcesFile,
		"-o", "Dir::Etc::sourceparts=-",
		"-o", "Dir::State::Lists=" + listsDir,
		"-o", "Dir::State::status=" + statusFile,
		"-o", "Dir::Cache=" + filepath.Join(root, "cache"),
		"-o", "Dir::Cache::Archives=" + archivesDir,
		"-o", "Acquire::Languages=none",
		"-o", "APT::Architecture=" + arch,
		"-o", "APT::Get::List-Cleanup=0",
	}
	if trusted {
		opts = append(opts, "-o", "Acquire::AllowInsecureRepositories=true")
	}
	return &aptEnv{opts: opts, timeout: 60 * time.Second}
}

func (e *aptEnv) run(t *testing.T, aptGet string, args ...string) {
	t.Helper()
	e.runIn(t, "", aptGet, args...)
}

func (e *aptEnv) runIn(t *testing.T, dir, aptGet string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), e.timeout)
	defer cancel()
	full := append(append([]string{}, e.opts...), args...)
	cmd := exec.CommandContext(ctx, aptGet, full...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("apt-get %v failed: %v\n%s", args, err, out.String())
	}
}
