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

// The npm real-client integration tests drive the actual `npm` CLI against an
// in-process hosted namespace (memblob backend, no Docker, no upstream). Each
// scenario is its own test so a CI failure points straight at the broken
// behavior. They skip when `npm` is not on PATH. Run locally with:
//
//	go test -tags=integration -run '^TestNPM' ./pkg/surface/npm

// TestNPMHostedPublishAndInstall publishes an unscoped package with `npm
// publish` and installs it back with `npm install`.
func TestNPMHostedPublishAndInstall(t *testing.T) {
	t.Parallel()

	c := newNPMClient(t)
	pkgDir := writePackage(t, "left-pad-demo", "1.0.0")
	c.run(t, pkgDir, "publish")

	installDir := writePackage(t, "consumer", "0.0.0")
	c.run(t, installDir, "install", "--no-audit", "--no-fund", "left-pad-demo@1.0.0")
	if _, err := os.Stat(filepath.Join(installDir, "node_modules", "left-pad-demo", "package.json")); err != nil {
		t.Fatalf("install did not materialize package: %v", err)
	}
}

// TestNPMScopedPublishAndInstall covers a scoped package end to end.
func TestNPMScopedPublishAndInstall(t *testing.T) {
	t.Parallel()

	c := newNPMClient(t)
	scopedDir := writePackage(t, "@oa/scoped-demo", "2.0.0")
	c.run(t, scopedDir, "publish", "--access", "public")

	installDir := writePackage(t, "consumer", "0.0.0")
	c.run(t, installDir, "install", "--no-audit", "--no-fund", "@oa/scoped-demo@2.0.0")
	if _, err := os.Stat(filepath.Join(installDir, "node_modules", "@oa", "scoped-demo", "package.json")); err != nil {
		t.Fatalf("scoped install did not materialize package: %v", err)
	}
}

// TestNPMDistTags exercises `npm dist-tag add` and `npm dist-tag ls`.
func TestNPMDistTags(t *testing.T) {
	t.Parallel()

	c := newNPMClient(t)
	pkgDir := writePackage(t, "left-pad-demo", "1.0.0")
	c.run(t, pkgDir, "publish")

	c.run(t, pkgDir, "dist-tag", "add", "left-pad-demo@1.0.0", "beta")
	out := c.output(t, pkgDir, "dist-tag", "ls", "left-pad-demo")
	if !strings.Contains(out, "beta: 1.0.0") || !strings.Contains(out, "latest: 1.0.0") {
		t.Fatalf("dist-tag ls missing expected tags:\n%s", out)
	}
}

// TestNPMProxyInstall drives `npm install` through a proxy namespace whose
// upstream is an in-process hosted open-artifact surface (no Docker, no public
// registry): a package is published to the hosted surface, then installed
// through the proxy, which pulls the packument and tarball through and caches
// them. A second install (from a fresh consumer dir) is served from the proxy's
// caches.
func TestNPMProxyInstall(t *testing.T) {
	t.Parallel()

	bin := requireNPM(t)

	// Hosted "upstream": publish a real (npm-produced) tarball to /team-a.
	hosted := newHarness(t, memblob.OpenBucket(nil), npm.Config{})
	hostedClient := &npmClient{bin: bin, userconfig: writeNpmrc(t, hosted.server.URL+"/team-a/")}
	pkgDir := writePackage(t, "left-pad-demo", "1.0.0")
	hostedClient.run(t, pkgDir, "publish")

	// Proxy whose upstream is the hosted namespace.
	proxy := newProxyHarness(t, memblob.OpenBucket(nil), npm.Config{},
		proxyNamespace("team-proxy", hosted.server.URL+"/team-a"))
	proxyClient := &npmClient{bin: bin, userconfig: writeNpmrc(t, proxy.server.URL+"/team-proxy/")}

	installDir := writePackage(t, "consumer", "0.0.0")
	proxyClient.run(t, installDir, "install", "--no-audit", "--no-fund", "left-pad-demo@1.0.0")
	if _, err := os.Stat(filepath.Join(installDir, "node_modules", "left-pad-demo", "package.json")); err != nil {
		t.Fatalf("proxy install did not materialize package: %v", err)
	}

	// A second install (fresh consumer) is served from the proxy's caches.
	installDir2 := writePackage(t, "consumer2", "0.0.0")
	proxyClient.run(t, installDir2, "install", "--no-audit", "--no-fund", "left-pad-demo@1.0.0")
	if _, err := os.Stat(filepath.Join(installDir2, "node_modules", "left-pad-demo", "package.json")); err != nil {
		t.Fatalf("second proxy install did not materialize package: %v", err)
	}
}

// npmClient bundles a running harness and the npm CLI configured to talk to it.
type npmClient struct {
	bin        string
	userconfig string
}

func newNPMClient(t *testing.T) *npmClient {
	t.Helper()
	bin := requireNPM(t)
	h := newHarness(t, memblob.OpenBucket(nil), npm.Config{})
	registry := h.server.URL + "/team-a/"
	return &npmClient{bin: bin, userconfig: writeNpmrc(t, registry)}
}

func (c *npmClient) run(t *testing.T, dir string, args ...string) {
	t.Helper()
	if _, err := c.exec(t, dir, args...); err != nil {
		t.Fatalf("npm %v: %v", args, err)
	}
}

func (c *npmClient) output(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := c.exec(t, dir, args...)
	if err != nil {
		t.Fatalf("npm %v: %v", args, err)
	}
	return out
}

func (c *npmClient) exec(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), c.bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"npm_config_userconfig="+c.userconfig,
		"npm_config_update_notifier=false",
		"npm_config_fund=false",
		"npm_config_audit=false",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		t.Logf("npm %v stdout:\n%s\nstderr:\n%s", args, stdout.String(), stderr.String())
	}
	return stdout.String(), err
}

func requireNPM(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("npm")
	if err != nil {
		t.Skipf("npm is not available: %v", err)
	}
	return bin
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

func writePackage(t *testing.T, name, version string) string {
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
