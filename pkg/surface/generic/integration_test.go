//go:build integration

package generic_test

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"

	"gocloud.dev/blob"
	"gocloud.dev/blob/fileblob"

	"github.com/yolocs/open-artifact/pkg/surface/generic"
)

// The generic format has no package-manager client tool, so its end-to-end test
// drives the in-process server with a plain HTTP client over the real
// blobstore/namespace/auth layers, exercising the full publish→list→download
// lifecycle on both mem:// and file:// buckets.
func TestGenericIntegration_Lifecycle(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, be backend) {
		h := newHarness(t, be.open(t), generic.Config{})

		// Create a version (and implicitly its package) with metadata.
		if resp := h.do(t, http.MethodPut, "/team-a/generic/packages/tool/versions/2.1.0",
			strings.NewReader(`{"annotations":{"channel":"stable"}}`), nil); resp.StatusCode != http.StatusCreated {
			t.Fatalf("create version = %d, want 201 (%s)", resp.StatusCode, body(t, resp))
		}

		base := "/team-a/generic/packages/tool/versions/2.1.0/files/"
		files := map[string]string{
			"tool-2.1.0.bin": "binary payload bytes",
			"README.txt":     "release notes for 2.1.0",
		}
		// Upload one file with an integrity header, one without.
		bin := files["tool-2.1.0.bin"]
		binSum := sha256.Sum256([]byte(bin))
		if resp := h.do(t, http.MethodPut, base+"tool-2.1.0.bin", strings.NewReader(bin), map[string]string{
			"Content-Type":      "application/octet-stream",
			"X-Checksum-Sha256": hex.EncodeToString(binSum[:]),
		}); resp.StatusCode != http.StatusCreated {
			t.Fatalf("upload binary = %d, want 201 (%s)", resp.StatusCode, body(t, resp))
		}
		if resp := h.do(t, http.MethodPut, base+"README.txt", strings.NewReader(files["README.txt"]), map[string]string{
			"Content-Type": "text/plain; charset=utf-8",
		}); resp.StatusCode != http.StatusCreated {
			t.Fatalf("upload readme = %d, want 201", resp.StatusCode)
		}

		// The package lists the version; the version lists both files.
		pkg := decode[pkgResp](t, h.do(t, http.MethodGet, "/team-a/generic/packages/tool", nil, nil))
		if len(pkg.Versions) != 1 || pkg.Versions[0] != "2.1.0" {
			t.Fatalf("package versions = %v, want [2.1.0]", pkg.Versions)
		}
		ver := decode[verResp](t, h.do(t, http.MethodGet, "/team-a/generic/packages/tool/versions/2.1.0", nil, nil))
		if ver.Annotations["channel"] != "stable" {
			t.Errorf("version annotations = %v, want channel=stable", ver.Annotations)
		}
		if len(ver.Files) != 2 {
			t.Errorf("version files = %v, want 2", ver.Files)
		}

		// File listing carries digests and content types.
		fl := decode[fileListResp](t, h.do(t, http.MethodGet, base[:len(base)-1], nil, nil))
		if len(fl.Files) != 2 {
			t.Fatalf("file list = %v, want 2", fl.Files)
		}

		// Every file downloads byte-for-byte.
		for name, want := range files {
			if got := body(t, h.do(t, http.MethodGet, base+name, nil, nil)); got != want {
				t.Errorf("download %q = %q, want %q", name, got, want)
			}
		}
	})
}

// TestGenericIntegration_SurvivesRestart proves hosted content is durable: a
// fresh server over the same file:// bucket still serves a previously uploaded
// file (no in-process state required).
func TestGenericIntegration_SurvivesRestart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	open := func() *blob.Bucket {
		b, err := fileblob.OpenBucket(dir, nil)
		if err != nil {
			t.Fatalf("fileblob.OpenBucket: %v", err)
		}
		t.Cleanup(func() { b.Close() })
		return b
	}

	content := "durable across restarts"
	path := "/team-a/generic/packages/persist/versions/1/files/blob.bin"

	first := newHarness(t, open(), generic.Config{})
	if resp := first.do(t, http.MethodPut, path, strings.NewReader(content), nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload = %d, want 201", resp.StatusCode)
	}

	// A new server over the same bucket dir serves the file without any shared
	// in-memory state.
	second := newHarness(t, open(), generic.Config{})
	resp := second.do(t, http.MethodGet, path, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download after restart = %d, want 200", resp.StatusCode)
	}
	if got := body(t, resp); got != content {
		t.Errorf("download after restart = %q, want %q", got, content)
	}
}
