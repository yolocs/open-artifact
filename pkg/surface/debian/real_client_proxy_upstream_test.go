//go:build integration

package debian_test

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gocloud.dev/blob/memblob"

	"github.com/yolocs/open-artifact/pkg/surface/debian"
)

// TestProxyLiveUpstream drives the real `apt-get` client through a proxy-mode
// namespace whose upstream is a real, signed Ubuntu mirror (archive.ubuntu.com
// or ports.ubuntu.com). Unlike the hermetic integration test it does NOT use
// `[trusted=yes]`: apt verifies the InRelease signature against the host's
// system keyring, so a passing `apt-get update` proves our proxy served the
// signed index byte-for-byte. `apt-get download hello` then proves a real .deb
// flows through the pull-through cache. It is gated behind the `integration`
// build tag (network-dependent) and skips when the mirror is unreachable.
func TestProxyLiveUpstream(t *testing.T) {
	t.Parallel()

	aptGet := requireApt(t)
	arch := dpkgArch(t)

	const suite = "noble"
	upstream := ubuntuMirror(arch)
	requireReachable(t, upstream+"/dists/"+suite+"/Release")

	h := newProxyHarness(t, memblob.OpenBucket(nil), debian.Config{}, proxyNS("team-proxy", upstream))
	proxyBase := h.server.URL + "/team-proxy/debian"

	root := t.TempDir()
	env := newAptEnv(t, root, proxyBase, arch, false, suite)
	env.timeout = 180 * time.Second // a real dist index is larger than the fake repo's.

	// apt-get update verifies the InRelease signature against the system keyring
	// on the bytes our proxy served — so this passing is the verbatim-passthrough
	// proof for the signed index.
	env.run(t, aptGet, "update")

	// A real .deb flows through the pull-through cache.
	downloadDir := filepath.Join(root, "downloads")
	if err := os.MkdirAll(downloadDir, 0o755); err != nil {
		t.Fatalf("mkdir downloads: %v", err)
	}
	env.runIn(t, downloadDir, aptGet, "download", "hello")

	deb := findDeb(t, downloadDir)
	if !strings.HasPrefix(string(deb), "!<arch>\n") {
		t.Fatalf("downloaded file is not a .deb ar archive (got %d bytes)", len(deb))
	}

	// Independently of apt, a direct proxy GET of an index file must match a
	// direct upstream fetch byte-for-byte.
	releasePath := "/dists/" + suite + "/Release"
	resp := get(t, h, "/team-proxy/debian"+releasePath)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy Release GET status = %d, want 200: %s", resp.StatusCode, readResp(t, resp))
	}
	viaProxy := readResp(t, resp)
	direct := fetch(t, upstream+releasePath)
	if viaProxy != direct {
		t.Fatalf("proxied Release differs from upstream (%d vs %d bytes)", len(viaProxy), len(direct))
	}
}

// ubuntuMirror returns the official Ubuntu mirror that carries the given dpkg
// architecture: archive.ubuntu.com for amd64/i386, ports.ubuntu.com otherwise.
func ubuntuMirror(arch string) string {
	switch arch {
	case "amd64", "i386":
		return "http://archive.ubuntu.com/ubuntu"
	default:
		return "http://ports.ubuntu.com/ubuntu-ports"
	}
}

func requireReachable(t *testing.T, url string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build reachability request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Skipf("upstream %s unreachable: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Skipf("upstream %s returned %d", url, resp.StatusCode)
	}
}

func fetch(t *testing.T, url string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request %s: %v", url, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return string(body)
}

func findDeb(t *testing.T, dir string) []byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read download dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".deb") {
			body, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatalf("read %s: %v", e.Name(), err)
			}
			return body
		}
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	t.Fatalf("no .deb in download dir (has %v)", names)
	return nil
}
