//go:build integration

package npm_test

import (
	"net/http"
	"testing"

	"gocloud.dev/blob/memblob"

	"github.com/yolocs/open-artifact/pkg/surface/npm"
)

// TestProxyLiveUpstream is a live-upstream smoke test against the real
// registry.npmjs.org. It uses isarray@1.0.0 — a tiny, dependency-free, long
// stable package — to exercise the real packument shape and tarball fill
// without a package-manager client. Run it with:
//
//	go test -tags=integration -run TestProxyLiveUpstream ./pkg/surface/npm
//
// Controllable scenarios (404/500, stale and synthesized fallback, filters,
// negative cache, scoped encoding) are covered by the in-process fakes in
// proxy_test.go.
func TestProxyLiveUpstream(t *testing.T) {
	t.Parallel()

	const (
		pkg     = "isarray"
		version = "1.0.0"
	)
	h := newProxyHarness(t, memblob.OpenBucket(nil), npm.Config{},
		proxyNamespace("team-proxy", "https://registry.npmjs.org"))

	// Packument: tarball URLs must be rewritten back to this registry.
	pkmt := decodeJSON(t, do(t, h, http.MethodGet, "/team-proxy/"+pkg))
	versions, _ := pkmt["versions"].(map[string]any)
	v, ok := versions[version].(map[string]any)
	if !ok {
		t.Fatalf("packument missing version %s: %v", version, pkmt)
	}
	dist, _ := v["dist"].(map[string]any)
	wantTarball := h.server.URL + "/team-proxy" + tarballPath(pkg, version)
	if dist["tarball"] != wantTarball {
		t.Fatalf("tarball URL = %v, want %q", dist["tarball"], wantTarball)
	}

	// Cold tarball download pulls through and fills the cache.
	dl := do(t, h, http.MethodGet, "/team-proxy"+tarballPath(pkg, version))
	if dl.StatusCode != http.StatusOK {
		t.Fatalf("download status = %d: %s", dl.StatusCode, readResp(t, dl))
	}
	if body := readResp(t, dl); len(body) == 0 {
		t.Fatalf("downloaded tarball is empty")
	}

	// Second download is served from the local cache.
	again := do(t, h, http.MethodGet, "/team-proxy"+tarballPath(pkg, version))
	if again.StatusCode != http.StatusOK {
		t.Fatalf("cached download status = %d: %s", again.StatusCode, readResp(t, again))
	}
	if body := readResp(t, again); len(body) == 0 {
		t.Fatalf("cached tarball is empty")
	}
}
