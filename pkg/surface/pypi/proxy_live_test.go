//go:build integration && pypiupstream

package pypi_test

import (
	"os"
	"path/filepath"
	"testing"

	"gocloud.dev/blob/memblob"

	"github.com/yolocs/open-artifact/pkg/surface/pypi"
)

// TestProxyLiveUpstreamPyPI is a smoke test against the real pypi.org. It is
// gated behind the pypiupstream build tag (in addition to integration) because
// it requires outbound network access. Run it locally with:
//
//	go test -tags=integration,pypiupstream -run TestProxyLiveUpstreamPyPI ./pkg/surface/pypi
//
// It installs a tiny, stable, pure-Python package through proxy mode.
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
	venv := filepath.Join(t.TempDir(), "venv")
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
