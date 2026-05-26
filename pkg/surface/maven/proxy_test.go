package maven_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gocloud.dev/blob"
	"gocloud.dev/blob/memblob"

	"github.com/yolocs/open-artifact/pkg/auth"
	"github.com/yolocs/open-artifact/pkg/namespace"
	"github.com/yolocs/open-artifact/pkg/proxy/filter"
	"github.com/yolocs/open-artifact/pkg/surface/integrationtest"
	"github.com/yolocs/open-artifact/pkg/surface/maven"
)

// fakeMaven is a configurable stand-in for an upstream Maven 2 repository. It
// serves files keyed by their repository-relative path
// (e.g. "com/example/demo/1.0.0/demo-1.0.0.jar"), counts hits per path, and
// supports per-path status overrides so tests can drive 404/500 and assert that
// cached artifacts are served without re-contacting upstream.
type fakeMaven struct {
	server *httptest.Server

	mu     sync.Mutex
	files  map[string][]byte
	status map[string]int // path => override status (0 => normal)
	hits   map[string]int
}

func newFakeMaven(t *testing.T) *fakeMaven {
	t.Helper()
	f := &fakeMaven{
		files:  map[string][]byte{},
		status: map[string]int{},
		hits:   map[string]int{},
	}
	f.server = httptest.NewServer(f)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeMaven) put(path string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[path] = body
}

func (f *fakeMaven) setStatus(path string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[path] = status
}

func (f *fakeMaven) hitCount(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[path]
}

func (f *fakeMaven) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/")
	f.hits[path]++
	if st := f.status[path]; st != 0 {
		w.WriteHeader(st)
		return
	}
	body, ok := f.files[path]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(body)
}

func proxyNS(name, upstream string, filters ...filter.Spec) *namespace.Namespace {
	ns := integrationtest.ProxyAnonymous(name, upstream)
	ns.Spec.Proxy.Filters = filters
	return ns
}

func newProxyHarness(t *testing.T, b *blob.Bucket, cfg maven.Config, nss ...*namespace.Namespace) *harness {
	t.Helper()
	ctx := t.Context()
	catalog, err := namespace.NewStore(b, "")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	for _, ns := range nss {
		if err := integrationtest.SeedNamespace(ctx, catalog, ns); err != nil {
			t.Fatalf("SeedNamespace(%s): %v", ns.Name, err)
		}
	}
	reg, err := namespace.NewRegistry(b, "", catalog)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	srv := httptest.NewServer(maven.Handler(reg, auth.AlwaysAnonymous{}, cfg))
	t.Cleanup(srv.Close)
	return &harness{server: srv, client: srv.Client()}
}

const (
	artifactRepoPath  = "com/example/demo/1.0.0/demo-1.0.0.jar"
	artifactProxyPath = "/team-proxy/maven2/com/example/demo/1.0.0/demo-1.0.0.jar"
	metadataRepoPath  = "com/example/demo/maven-metadata.xml"
	metadataProxyPath = "/team-proxy/maven2/com/example/demo/maven-metadata.xml"
)

func TestProxyArtifactMetadataPassthrough(t *testing.T) {
	t.Parallel()
	up := newFakeMaven(t)
	metadata := []byte("<metadata><groupId>com.example</groupId><artifactId>demo</artifactId></metadata>")
	up.put(metadataRepoPath, metadata)
	h := newProxyHarness(t, memblob.OpenBucket(nil), maven.Config{}, proxyNS("team-proxy", up.server.URL))

	for i := 0; i < 2; i++ {
		resp := get(t, h, metadataProxyPath)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("metadata GET status = %d: %s", resp.StatusCode, readResp(t, resp))
		}
		if ct := resp.Header.Get("Content-Type"); ct != "application/xml" {
			t.Fatalf("content type = %q, want application/xml", ct)
		}
		if body := readResp(t, resp); body != string(metadata) {
			t.Fatalf("metadata body = %q", body)
		}
	}
	// Metadata is live: every request reaches upstream (no caching).
	if got := up.hitCount(metadataRepoPath); got != 2 {
		t.Fatalf("upstream metadata hits = %d, want 2 (live passthrough)", got)
	}
}

func TestProxySnapshotMetadataPassthrough(t *testing.T) {
	t.Parallel()
	up := newFakeMaven(t)
	snapPath := "com/example/demo/1.0.0-SNAPSHOT/maven-metadata.xml"
	metadata := []byte("<metadata><versioning><snapshot><timestamp>20260101.000000</timestamp></snapshot></versioning></metadata>")
	up.put(snapPath, metadata)
	h := newProxyHarness(t, memblob.OpenBucket(nil), maven.Config{}, proxyNS("team-proxy", up.server.URL))

	resp := get(t, h, "/team-proxy/maven2/"+snapPath)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("snapshot metadata status = %d: %s", resp.StatusCode, readResp(t, resp))
	}
	if body := readResp(t, resp); body != string(metadata) {
		t.Fatalf("snapshot metadata body = %q", body)
	}
}

func TestProxyArtifactMissFillAndRestart(t *testing.T) {
	t.Parallel()
	for _, be := range backends() {
		be := be
		t.Run(be.name, func(t *testing.T) {
			t.Parallel()
			bucket := be.open(t)
			jar := []byte("jar bytes from upstream")
			up := newFakeMaven(t)
			up.put(artifactRepoPath, jar)

			h := newProxyHarness(t, bucket, maven.Config{}, proxyNS("team-proxy", up.server.URL))

			// Cold GET fetches upstream and fills the cache.
			cold := get(t, h, artifactProxyPath)
			if cold.StatusCode != http.StatusOK {
				t.Fatalf("cold GET status = %d: %s", cold.StatusCode, readResp(t, cold))
			}
			if got := readResp(t, cold); got != string(jar) {
				t.Fatalf("cold body = %q, want %q", got, jar)
			}
			if ct := cold.Header.Get("Content-Type"); ct != "application/octet-stream" {
				t.Fatalf("content type = %q", ct)
			}
			if got := up.hitCount(artifactRepoPath); got != 1 {
				t.Fatalf("upstream hits after cold = %d, want 1", got)
			}

			// Upstream goes away; a warm GET is served from the blob cache.
			up.setStatus(artifactRepoPath, http.StatusInternalServerError)
			warm := get(t, h, artifactProxyPath)
			if warm.StatusCode != http.StatusOK {
				t.Fatalf("warm GET status = %d: %s", warm.StatusCode, readResp(t, warm))
			}
			if got := readResp(t, warm); got != string(jar) {
				t.Fatalf("warm body = %q, want %q", got, jar)
			}
			if got := up.hitCount(artifactRepoPath); got != 1 {
				t.Fatalf("upstream hits after warm = %d, want 1 (served from cache)", got)
			}

			// Restart: a fresh server/registry over the same bucket still serves the
			// cached artifact with upstream down.
			h2 := newProxyHarness(t, bucket, maven.Config{}, proxyNS("team-proxy", up.server.URL))
			restarted := get(t, h2, artifactProxyPath)
			if restarted.StatusCode != http.StatusOK {
				t.Fatalf("post-restart status = %d: %s", restarted.StatusCode, readResp(t, restarted))
			}
			if got := readResp(t, restarted); got != string(jar) {
				t.Fatalf("post-restart body = %q, want %q", got, jar)
			}
		})
	}
}

func TestProxyChecksumFillFromUpstream(t *testing.T) {
	t.Parallel()
	up := newFakeMaven(t)
	checksum := []byte("0123456789abcdef0123456789abcdef01234567")
	up.put(artifactRepoPath+".sha1", checksum)
	h := newProxyHarness(t, memblob.OpenBucket(nil), maven.Config{}, proxyNS("team-proxy", up.server.URL))

	// Target jar is NOT cached, so the checksum is fetched from upstream.
	cold := get(t, h, artifactProxyPath+".sha1")
	if cold.StatusCode != http.StatusOK {
		t.Fatalf("checksum GET status = %d: %s", cold.StatusCode, readResp(t, cold))
	}
	if got := readResp(t, cold); got != string(checksum) {
		t.Fatalf("checksum body = %q, want %q", got, checksum)
	}
	if ct := cold.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("checksum content type = %q, want text/plain", ct)
	}

	// Cached as a normal file: upstream down, still served.
	up.setStatus(artifactRepoPath+".sha1", http.StatusInternalServerError)
	warm := get(t, h, artifactProxyPath+".sha1")
	if warm.StatusCode != http.StatusOK {
		t.Fatalf("warm checksum status = %d: %s", warm.StatusCode, readResp(t, warm))
	}
	if got := readResp(t, warm); got != string(checksum) {
		t.Fatalf("warm checksum body = %q", got)
	}
	if got := up.hitCount(artifactRepoPath + ".sha1"); got != 1 {
		t.Fatalf("upstream checksum hits = %d, want 1 (cached)", got)
	}
}

func TestProxyChecksumSynthesisFromCachedTarget(t *testing.T) {
	t.Parallel()
	up := newFakeMaven(t)
	jar := []byte("jar bytes for checksum synthesis")
	up.put(artifactRepoPath, jar)
	// Upstream deliberately has no .sha1 companion; the proxy must synthesize it.
	h := newProxyHarness(t, memblob.OpenBucket(nil), maven.Config{}, proxyNS("team-proxy", up.server.URL))

	// Fill the jar first.
	if resp := get(t, h, artifactProxyPath); resp.StatusCode != http.StatusOK {
		t.Fatalf("jar fill status = %d: %s", resp.StatusCode, readResp(t, resp))
	} else {
		_ = readResp(t, resp)
	}

	resp := get(t, h, artifactProxyPath+".sha1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("synthesized checksum status = %d: %s", resp.StatusCode, readResp(t, resp))
	}
	if got := readResp(t, resp); got != sha1Hex(string(jar)) {
		t.Fatalf("synthesized sha1 = %q, want %q", got, sha1Hex(string(jar)))
	}
	// Synthesis never touched upstream for the .sha1.
	if got := up.hitCount(artifactRepoPath + ".sha1"); got != 0 {
		t.Fatalf("upstream .sha1 hits = %d, want 0 (synthesized locally)", got)
	}

	// And it was cached: served again with upstream entirely down.
	up.setStatus(artifactRepoPath+".sha1", http.StatusInternalServerError)
	again := get(t, h, artifactProxyPath+".sha1")
	if again.StatusCode != http.StatusOK {
		t.Fatalf("cached synthesized checksum status = %d", again.StatusCode)
	}
	if got := readResp(t, again); got != sha1Hex(string(jar)) {
		t.Fatalf("cached synthesized sha1 = %q", got)
	}
}

func TestProxyFilterDeny(t *testing.T) {
	t.Parallel()
	up := newFakeMaven(t)
	up.put(artifactRepoPath, []byte("denied jar"))
	deny := filter.Spec{Kind: filter.KindDeny, Patterns: []string{"com.example:demo"}}
	h := newProxyHarness(t, memblob.OpenBucket(nil), maven.Config{}, proxyNS("team-proxy", up.server.URL, deny))

	resp := get(t, h, artifactProxyPath)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("denied artifact status = %d, want 404: %s", resp.StatusCode, readResp(t, resp))
	}
	_ = readResp(t, resp)
	// A denied artifact is never fetched from upstream.
	if got := up.hitCount(artifactRepoPath); got != 0 {
		t.Fatalf("upstream hits for denied artifact = %d, want 0", got)
	}
}

func TestProxyDelayFilter(t *testing.T) {
	t.Parallel()
	delay := filter.Spec{Kind: filter.KindDelay, MinAge: "8760h"} // 1 year

	t.Run("old enough allowed", func(t *testing.T) {
		t.Parallel()
		up := newFakeMaven(t)
		up.put(artifactRepoPath, []byte("old jar"))
		up.put(metadataRepoPath, []byte("<metadata><versioning><lastUpdated>20000101000000</lastUpdated></versioning></metadata>"))
		h := newProxyHarness(t, memblob.OpenBucket(nil), maven.Config{}, proxyNS("team-proxy", up.server.URL, delay))
		resp := get(t, h, artifactProxyPath)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("old artifact status = %d, want 200: %s", resp.StatusCode, readResp(t, resp))
		}
		_ = readResp(t, resp)
	})

	t.Run("too fresh denied", func(t *testing.T) {
		t.Parallel()
		up := newFakeMaven(t)
		up.put(artifactRepoPath, []byte("fresh jar"))
		recent := time.Now().UTC().Format("20060102150405")
		up.put(metadataRepoPath, []byte("<metadata><versioning><lastUpdated>"+recent+"</lastUpdated></versioning></metadata>"))
		h := newProxyHarness(t, memblob.OpenBucket(nil), maven.Config{}, proxyNS("team-proxy", up.server.URL, delay))
		resp := get(t, h, artifactProxyPath)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("fresh artifact status = %d, want 404: %s", resp.StatusCode, readResp(t, resp))
		}
		_ = readResp(t, resp)
	})

	t.Run("unknown time fails closed", func(t *testing.T) {
		t.Parallel()
		up := newFakeMaven(t)
		up.put(artifactRepoPath, []byte("unknown jar"))
		// No metadata published, so lastUpdated is unavailable.
		h := newProxyHarness(t, memblob.OpenBucket(nil), maven.Config{}, proxyNS("team-proxy", up.server.URL, delay))
		resp := get(t, h, artifactProxyPath)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("unknown-time artifact status = %d, want 404 (fail closed): %s", resp.StatusCode, readResp(t, resp))
		}
		_ = readResp(t, resp)
	})
}

func TestProxyUpstream404(t *testing.T) {
	t.Parallel()
	up := newFakeMaven(t)
	h := newProxyHarness(t, memblob.OpenBucket(nil), maven.Config{}, proxyNS("team-proxy", up.server.URL))

	artifact := get(t, h, artifactProxyPath)
	if artifact.StatusCode != http.StatusNotFound {
		t.Fatalf("missing artifact status = %d, want 404: %s", artifact.StatusCode, readResp(t, artifact))
	}
	_ = readResp(t, artifact)

	metadata := get(t, h, metadataProxyPath)
	if metadata.StatusCode != http.StatusNotFound {
		t.Fatalf("missing metadata status = %d, want 404: %s", metadata.StatusCode, readResp(t, metadata))
	}
	_ = readResp(t, metadata)
}

func TestProxyUpstream500(t *testing.T) {
	t.Parallel()
	up := newFakeMaven(t)
	up.setStatus(artifactRepoPath, http.StatusInternalServerError)
	up.setStatus(metadataRepoPath, http.StatusInternalServerError)
	h := newProxyHarness(t, memblob.OpenBucket(nil), maven.Config{}, proxyNS("team-proxy", up.server.URL))

	// Artifact upstream 5xx maps to 502 (bad gateway), matching the npm/pypi proxies.
	artifact := get(t, h, artifactProxyPath)
	if artifact.StatusCode != http.StatusBadGateway {
		t.Fatalf("artifact 500 status = %d, want 502: %s", artifact.StatusCode, readResp(t, artifact))
	}
	_ = readResp(t, artifact)

	// Metadata upstream 5xx maps to 503 (upstream unavailable).
	metadata := get(t, h, metadataProxyPath)
	if metadata.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("metadata 500 status = %d, want 503: %s", metadata.StatusCode, readResp(t, metadata))
	}
	_ = readResp(t, metadata)
}

func TestProxyOversizedMetadata(t *testing.T) {
	t.Parallel()
	up := newFakeMaven(t)
	up.put(metadataRepoPath, []byte(strings.Repeat("x", 4096)))
	h := newProxyHarness(t, memblob.OpenBucket(nil), maven.Config{ProxyMetadataMaxBytes: 8}, proxyNS("team-proxy", up.server.URL))

	resp := get(t, h, metadataProxyPath)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("oversized metadata status = %d, want 502: %s", resp.StatusCode, readResp(t, resp))
	}
	_ = readResp(t, resp)
}

func TestProxyNegativeCache(t *testing.T) {
	t.Parallel()
	up := newFakeMaven(t)
	// 30s default negative TTL comfortably covers the test.
	h := newProxyHarness(t, memblob.OpenBucket(nil), maven.Config{}, proxyNS("team-proxy", up.server.URL))

	for i := 0; i < 3; i++ {
		resp := get(t, h, artifactProxyPath)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("miss %d status = %d, want 404: %s", i, resp.StatusCode, readResp(t, resp))
		}
		_ = readResp(t, resp)
	}
	if got := up.hitCount(artifactRepoPath); got != 1 {
		t.Fatalf("upstream hits = %d, want 1 (negative cache absorbs the rest)", got)
	}
}

func TestProxyWriteRoutesReturn405(t *testing.T) {
	t.Parallel()
	up := newFakeMaven(t)
	h := newProxyHarness(t, memblob.OpenBucket(nil), maven.Config{}, proxyNS("team-proxy", up.server.URL))

	for _, method := range []string{http.MethodPut, http.MethodPost} {
		resp := request(t, h, method, artifactProxyPath, strings.NewReader("bytes"))
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("%s status = %d, want 405: %s", method, resp.StatusCode, readResp(t, resp))
		}
		if allow := resp.Header.Get("Allow"); !strings.Contains(allow, http.MethodGet) {
			t.Fatalf("%s Allow header = %q, want it to list GET", method, allow)
		}
		_ = readResp(t, resp)
	}
}

func TestProxyArchetypeCatalogLocalOnly(t *testing.T) {
	t.Parallel()
	up := newFakeMaven(t)
	// Even if upstream had a catalog, proxy mode must not fetch it in v1.
	up.put("archetype-catalog.xml", []byte("<archetype-catalog/>"))
	h := newProxyHarness(t, memblob.OpenBucket(nil), maven.Config{}, proxyNS("team-proxy", up.server.URL))

	resp := get(t, h, "/team-proxy/maven2/archetype-catalog.xml")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("archetype catalog status = %d, want 404 (local-only): %s", resp.StatusCode, readResp(t, resp))
	}
	_ = readResp(t, resp)
	if got := up.hitCount("archetype-catalog.xml"); got != 0 {
		t.Fatalf("upstream archetype hits = %d, want 0", got)
	}

	write := put(t, h, "/team-proxy/maven2/archetype-catalog.xml", "<archetype-catalog/>")
	if write.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("archetype write status = %d, want 405: %s", write.StatusCode, readResp(t, write))
	}
	_ = readResp(t, write)
}
