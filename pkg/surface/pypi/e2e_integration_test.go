//go:build integration

package pypi

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// buildWheel returns the bytes of a minimal but valid wheel for dist/version.
// pip parses the wheel filename for compatibility tags and the dist-info
// METADATA for the project name; with --no-deps it does not resolve
// dependencies, so a minimal METADATA/WHEEL/RECORD is enough to download.
func buildWheel(t *testing.T, dist, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	distInfo := fmt.Sprintf("%s-%s.dist-info", dist, version)

	write := func(name, content string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := io.WriteString(w, content); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}

	write(distInfo+"/METADATA", fmt.Sprintf("Metadata-Version: 2.1\nName: %s\nVersion: %s\nSummary: test\n", dist, version))
	write(distInfo+"/WHEEL", "Wheel-Version: 1.0\nGenerator: open-artifact-test\nRoot-Is-Purelib: true\nTag: py3-none-any\n")
	write(distInfo+"/RECORD", "")

	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// pipDownload runs `pip download` against indexURL into a fresh dest dir,
// returning the dir. It isolates pip from the network and any user config.
func pipDownload(t *testing.T, indexURL, pkg string) string {
	t.Helper()
	pip, err := exec.LookPath("pip3")
	if err != nil {
		t.Skipf("pip3 not available: %v", err)
	}
	dest := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, pip, "download",
		"--no-deps",
		"--no-cache-dir",
		"--disable-pip-version-check",
		"--index-url", indexURL,
		"--dest", dest,
		pkg,
	)
	cmd.Env = append(os.Environ(),
		"PIP_NO_CACHE_DIR=1",
		"PIP_DISABLE_PIP_VERSION_CHECK=1",
		"PIP_REQUIRE_VIRTUALENV=false",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pip download failed: %v\n%s", err, out)
	}
	t.Logf("pip download output:\n%s", out)
	return dest
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// TestEndToEndPublishedPath publishes a wheel via the upload endpoint, then
// uses a real `pip download` against the surface's /simple/ index to fetch it.
func TestEndToEndPublishedPath(t *testing.T) {
	t.Parallel()
	h := NewHandler(newStore(t), nil)
	srv, client := serve(t, h)

	wheel := buildWheel(t, "samplepkg", "1.0.0")
	filename := "samplepkg-1.0.0-py3-none-any.whl"
	resp := upload(t, srv, client, "samplepkg", "1.0.0", filename, wheel)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upload status = %d, want 200", resp.StatusCode)
	}

	dest := pipDownload(t, srv.URL+"/simple/", "samplepkg")
	names := listDir(t, dest)
	found := false
	for _, n := range names {
		if n == filename {
			found = true
		}
	}
	if !found {
		t.Errorf("pip did not download %q; got %v", filename, names)
	}

	// The downloaded bytes must match what we published.
	gotBytes, err := os.ReadFile(filepath.Join(dest, filename))
	if err == nil && sha256hex(gotBytes) != sha256hex(wheel) {
		t.Errorf("downloaded wheel sha256 mismatch")
	}
}

// TestEndToEndProxyPath stands up a fake upstream advertising a wheel, then
// runs `pip download` against the surface in proxy mode. The surface resolves
// the file from upstream, caches it, and serves it to pip.
func TestEndToEndProxyPath(t *testing.T) {
	t.Parallel()

	wheel := buildWheel(t, "proxypkg", "2.0.0")
	filename := "proxypkg-2.0.0-py3-none-any.whl"
	hash := sha256hex(wheel)

	// Fake upstream: PEP 691 JSON index + file bytes.
	var upstreamURL string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /simple/{package}/{$}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("package") != "proxypkg" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", contentTypeJSONv1)
		fmt.Fprintf(w, `{"meta":{"api-version":"1.0"},"name":"proxypkg","files":[{"filename":%q,"url":%q,"hashes":{"sha256":%q}}]}`,
			filename, upstreamURL+"/files/"+filename, hash)
	})
	mux.HandleFunc("GET /files/{filename}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("filename") != filename {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(wheel)
	})
	upstream := httptest.NewServer(mux)
	defer upstream.Close()
	upstreamURL = upstream.URL

	up, err := NewUpstreamClient(upstream.URL)
	if err != nil {
		t.Fatalf("NewUpstreamClient: %v", err)
	}
	store := newStore(t)
	h := NewHandler(store, up)
	srv, _ := serve(t, h)

	dest := pipDownload(t, srv.URL+"/simple/", "proxypkg")
	gotBytes, err := os.ReadFile(filepath.Join(dest, filename))
	if err != nil {
		t.Fatalf("pip did not download %q: %v (dir: %v)", filename, err, listDir(t, dest))
	}
	if sha256hex(gotBytes) != hash {
		t.Errorf("downloaded wheel sha256 mismatch via proxy")
	}

	// The proxy must have cached the wheel into the Store.
	f := store.Package("proxypkg").Version("2.0.0").File(filename)
	if exists, _ := f.Exists(t.Context()); !exists {
		t.Errorf("proxy download did not populate the Store cache")
	}
}
