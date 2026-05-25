//go:build integration

package npm_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gocloud.dev/blob/memblob"

	"github.com/yolocs/open-artifact/pkg/surface/npm"
)

// TestNPMRealClient drives the real `npm` CLI against an in-process hosted
// namespace: publish, install, scoped publish/install, and dist-tag add/ls for
// both scoped and unscoped packages. Run locally with:
//
//	go test -tags=integration -run TestNPMRealClient ./pkg/surface/npm
//
// It needs the `npm` binary on PATH; it skips otherwise. No Docker or upstream
// registry is required — the backend is memblob and the client talks to the
// in-process server.
func TestNPMRealClient(t *testing.T) {
	t.Parallel()

	npmBin := requireNPM(t)
	h := newHarness(t, memblob.OpenBucket(nil), npm.Config{})
	registry := h.server.URL + "/team-a/"
	userconfig := writeNpmrc(t, registry)

	// Unscoped publish + install.
	pkgDir := writePackage(t, "left-pad-demo", "1.0.0", false)
	npmRun(t, npmBin, userconfig, pkgDir, "publish")

	installDir := writePackage(t, "consumer", "0.0.0", false)
	npmRun(t, npmBin, userconfig, installDir, "install", "--no-audit", "--no-fund", "left-pad-demo@1.0.0")
	if _, err := os.Stat(filepath.Join(installDir, "node_modules", "left-pad-demo", "package.json")); err != nil {
		t.Fatalf("unscoped install did not materialize package: %v", err)
	}

	// dist-tag add + ls.
	npmRun(t, npmBin, userconfig, pkgDir, "dist-tag", "add", "left-pad-demo@1.0.0", "beta")
	out := npmOutput(t, npmBin, userconfig, pkgDir, "dist-tag", "ls", "left-pad-demo")
	if !strings.Contains(out, "beta: 1.0.0") || !strings.Contains(out, "latest: 1.0.0") {
		t.Fatalf("dist-tag ls missing expected tags:\n%s", out)
	}

	// Scoped publish + install.
	scopedDir := writePackage(t, "@oa/scoped-demo", "2.0.0", true)
	npmRun(t, npmBin, userconfig, scopedDir, "publish", "--access", "public")
	npmRun(t, npmBin, userconfig, installDir, "install", "--no-audit", "--no-fund", "@oa/scoped-demo@2.0.0")
	if _, err := os.Stat(filepath.Join(installDir, "node_modules", "@oa", "scoped-demo", "package.json")); err != nil {
		t.Fatalf("scoped install did not materialize package: %v", err)
	}
}

func requireNPM(t *testing.T) string {
	t.Helper()
	npmBin, err := exec.LookPath("npm")
	if err != nil {
		t.Skipf("npm is not available: %v", err)
	}
	return npmBin
}

// writeNpmrc writes a userconfig that points npm at the given registry and
// supplies an anonymous auth token (the harness uses AlwaysAnonymous).
func writeNpmrc(t *testing.T, registry string) string {
	t.Helper()
	noScheme := strings.TrimPrefix(registry, "http:")
	content := "registry=" + registry + "\n" +
		noScheme + ":_authToken=anonymous\n" +
		"strict-ssl=false\n"
	path := filepath.Join(t.TempDir(), ".npmrc")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write .npmrc: %v", err)
	}
	return path
}

func writePackage(t *testing.T, name, version string, scoped bool) string {
	t.Helper()
	dir := t.TempDir()
	pkg := map[string]any{
		"name":        name,
		"version":     version,
		"description": "open-artifact npm integration package",
		"main":        "index.js",
	}
	body, err := json.MarshalIndent(pkg, "", "  ")
	if err != nil {
		t.Fatalf("marshal package.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), body, 0o644); err != nil {
		t.Fatalf("write package.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte("module.exports = 'ok';\n"), 0o644); err != nil {
		t.Fatalf("write index.js: %v", err)
	}
	return dir
}

func npmEnv(userconfig string) []string {
	return append(os.Environ(),
		"npm_config_userconfig="+userconfig,
		"npm_config_update_notifier=false",
		"npm_config_fund=false",
		"npm_config_audit=false",
	)
}

func npmRun(t *testing.T, npmBin, userconfig, dir string, args ...string) {
	t.Helper()
	if _, err := runNPM(t, npmBin, userconfig, dir, args...); err != nil {
		t.Fatalf("npm %v: %v", args, err)
	}
}

func npmOutput(t *testing.T, npmBin, userconfig, dir string, args ...string) string {
	t.Helper()
	out, err := runNPM(t, npmBin, userconfig, dir, args...)
	if err != nil {
		t.Fatalf("npm %v: %v", args, err)
	}
	return out
}

func runNPM(t *testing.T, npmBin, userconfig, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), npmBin, args...)
	cmd.Dir = dir
	cmd.Env = npmEnv(userconfig)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		t.Logf("npm %v stdout:\n%s\nstderr:\n%s", args, stdout.String(), stderr.String())
	}
	return stdout.String(), err
}
