//go:build integration

package pypi_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gocloud.dev/blob"
	"gocloud.dev/blob/fileblob"
	"gocloud.dev/blob/memblob"

	"github.com/yolocs/open-artifact/pkg/surface/pypi"
)

// TestPyPIProxyDownloadRestart proves that artifacts pulled through a proxy
// namespace are durable: after a simulated process restart (a fresh bucket
// handle, registry, and handler over the same file:// directory) with the
// upstream unavailable, a previously cached file still serves.
func TestPyPIProxyDownloadRestart(t *testing.T) {
	t.Parallel()

	wheel := upstreamFile{filename: "demo-1.0.0-py3-none-any.whl", body: []byte("durable wheel bytes")}
	up := newFakeUpstream(t, "demo", []upstreamFile{wheel}, false)

	dir := t.TempDir()
	openBucket := func() *blob.Bucket {
		b, err := fileblob.OpenBucket(dir, nil)
		if err != nil {
			t.Fatalf("fileblob.OpenBucket: %v", err)
		}
		return b
	}

	// Phase 1: a fresh proxy fills the cache from upstream.
	b1 := openBucket()
	h1 := newProxyHarness(t, b1, pypi.Config{ProxyIndexCacheTTL: -1}, proxyNamespace("team-proxy", up.server.URL))
	url := "/team-proxy/packages/demo/1.0.0/" + wheel.filename
	dl := get(t, h1, url, "")
	if dl.StatusCode != http.StatusOK {
		t.Fatalf("cold download status = %d: %s", dl.StatusCode, readResp(t, dl))
	}
	if got := readResp(t, dl); got != string(wheel.body) {
		t.Fatalf("cold download body = %q, want %q", got, wheel.body)
	}
	b1.Close()

	// Upstream is now completely unavailable.
	up.setSimpleStatus(http.StatusInternalServerError)
	up.mu.Lock()
	up.fileStatus[wheel.filename] = http.StatusInternalServerError
	up.mu.Unlock()

	// Phase 2: a brand-new process (fresh bucket, registry, handler) over the
	// same directory still serves the cached file.
	b2 := openBucket()
	defer b2.Close()
	h2 := newProxyHarness(t, b2, pypi.Config{ProxyIndexCacheTTL: -1}, proxyNamespace("team-proxy", up.server.URL))
	after := get(t, h2, url, "")
	if after.StatusCode != http.StatusOK {
		t.Fatalf("post-restart download status = %d, want 200: %s", after.StatusCode, readResp(t, after))
	}
	if got := readResp(t, after); got != string(wheel.body) {
		t.Fatalf("post-restart body = %q, want %q", got, wheel.body)
	}
}

// TestPyPIProxyDownload drives a real pip client through a proxy namespace
// backed by an in-process fake PyPI upstream: the first install pulls the wheel
// through and caches it; the second install succeeds with the upstream
// unavailable because the file is cached.
func TestPyPIProxyDownload(t *testing.T) {
	t.Parallel()

	python := requirePython(t)
	wheel := buildWheel(t, "demo-pkg", "demo_pkg", "0.1.0")
	filename := filepath.Base(wheel)
	up := newFakeUpstream(t, "demo-pkg", []upstreamFile{{filename: filename, body: mustRead(t, wheel)}}, false)
	h := newProxyHarness(t, memblob.OpenBucket(nil), pypi.Config{}, proxyNamespace("team-proxy", up.server.URL))

	indexURL := h.server.URL + "/team-proxy/simple/"
	pipEnv := append(os.Environ(),
		"PIP_DISABLE_PIP_VERSION_CHECK=1",
		"PIP_NO_INPUT=1",
		"PYTHONNOUSERSITE=1",
	)

	install := func(venv string) {
		runCmd(t, "", python, "-m", "venv", venv)
		vpy := venvPython(t, venv)
		runCmdEnv(t, pipEnv, "", vpy, "-m", "pip", "install",
			"--no-deps", "--no-cache-dir",
			"--index-url", indexURL,
			"--trusted-host", hostOnly(t, indexURL),
			"demo-pkg==0.1.0",
		)
		runCmdEnv(t, pipEnv, "", vpy, "-c", `import demo_pkg; assert demo_pkg.hello() == "ok"`)
	}

	// First install pulls the wheel through and caches it.
	install(filepath.Join(t.TempDir(), "venv1"))

	// With the upstream unavailable, a fresh install still succeeds from cache.
	up.setSimpleStatus(http.StatusInternalServerError)
	up.mu.Lock()
	up.fileStatus[filename] = http.StatusInternalServerError
	up.mu.Unlock()
	install(filepath.Join(t.TempDir(), "venv2"))

	// The pulled file is indexed under the proxy namespace.
	if body := readResp(t, get(t, h, "/team-proxy/simple/demo-pkg/", "")); !strings.Contains(body, filename) {
		t.Fatalf("proxy simple index missing cached file: %s", body)
	}
}
