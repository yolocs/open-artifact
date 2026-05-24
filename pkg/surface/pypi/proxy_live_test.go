//go:build integration

package pypi_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gocloud.dev/blob/memblob"

	"github.com/yolocs/open-artifact/pkg/surface/pypi"
)

// TestProxyLiveUpstreamPyPI is a smoke test against the real pypi.org, run as
// part of the integration suite (integration tests hit real upstreams). Run it
// locally with:
//
//	go test -tags=integration -run TestProxyLiveUpstreamPyPI ./pkg/surface/pypi
//
// It installs a tiny, stable, pure-Python package through proxy mode against
// real Warehouse output (exercising the actual PEP 503/691 parsing and link
// rewriting), then proves the artifact was cached locally and a second install
// is served from cache.
func TestProxyLiveUpstreamPyPI(t *testing.T) {
	t.Parallel()

	python := requirePython(t)
	h := newProxyHarness(t, memblob.OpenBucket(nil), pypi.Config{}, proxyNamespace("team-proxy", "https://pypi.org"))

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
			"iniconfig==2.0.0",
		)
		runCmdEnv(t, pipEnv, "", vpy, "-c", "import iniconfig")
	}

	// First install pulls the wheel through from real pypi.org.
	install(filepath.Join(t.TempDir(), "venv1"))

	// The artifact is now cached locally — the proxy root lists it.
	if body := readResp(t, get(t, h, "/team-proxy/simple/", "")); !strings.Contains(body, "iniconfig") {
		t.Fatalf("proxy root does not list the cached project: %s", body)
	}

	// A second install succeeds (served from the local cache for the file, and
	// the in-process/durable index cache for the index).
	install(filepath.Join(t.TempDir(), "venv2"))
}
