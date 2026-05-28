//go:build integration

package debian_test

import (
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
// flows through the pull-through cache.
//
// Controllable scenarios (404/5xx, stale fallback, restart, negative cache,
// filters) are covered by the in-process fakes in proxy_test.go.
func TestProxyLiveUpstream(t *testing.T) {
	t.Parallel()

	aptGet := requireApt(t)
	arch := dpkgArch(t)

	const suite = "noble"
	upstream := ubuntuMirror(arch)

	h := newProxyHarness(t, memblob.OpenBucket(nil), debian.Config{}, proxyNS("team-proxy", upstream))

	root := t.TempDir()
	env := newAptEnv(t, root, h.server.URL+"/team-proxy/debian", arch, false, suite)
	env.timeout = 180 * time.Second // a real dist index is larger than the fake repo's.

	// apt-get update verifies the InRelease signature against the system keyring
	// on the bytes our proxy served — the verbatim-passthrough proof on real data.
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
