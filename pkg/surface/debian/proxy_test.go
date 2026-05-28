package debian_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"gocloud.dev/blob"
	"gocloud.dev/blob/fileblob"
	"gocloud.dev/blob/memblob"

	"github.com/yolocs/open-artifact/pkg/auth"
	"github.com/yolocs/open-artifact/pkg/namespace"
	"github.com/yolocs/open-artifact/pkg/proxy/filter"
	"github.com/yolocs/open-artifact/pkg/surface/debian"
	"github.com/yolocs/open-artifact/pkg/surface/integrationtest"
)

// fakeDebian is a configurable stand-in for an upstream APT repository. It
// serves files keyed by their repository-relative path, counts hits per path,
// and supports per-path status overrides so tests can drive 404/500 and assert
// that cached content is served without re-contacting upstream.
type fakeDebian struct {
	server *httptest.Server

	mu     sync.Mutex
	files  map[string][]byte
	status map[string]int // path => override status (0 => normal)
	hits   map[string]int
}

func newFakeDebian(t *testing.T) *fakeDebian {
	t.Helper()
	f := &fakeDebian{
		files:  map[string][]byte{},
		status: map[string]int{},
		hits:   map[string]int{},
	}
	f.server = httptest.NewServer(f)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeDebian) put(path string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[path] = body
}

func (f *fakeDebian) setStatus(path string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[path] = status
}

func (f *fakeDebian) hitCount(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[path]
}

func (f *fakeDebian) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

type backend struct {
	name string
	open func(t *testing.T) *blob.Bucket
}

func backends() []backend {
	return []backend{
		{
			name: "memblob",
			open: func(t *testing.T) *blob.Bucket {
				t.Helper()
				b := memblob.OpenBucket(nil)
				t.Cleanup(func() { b.Close() })
				return b
			},
		},
		{
			name: "fileblob",
			open: func(t *testing.T) *blob.Bucket {
				t.Helper()
				b, err := fileblob.OpenBucket(t.TempDir(), nil)
				if err != nil {
					t.Fatalf("fileblob.OpenBucket: %v", err)
				}
				t.Cleanup(func() { b.Close() })
				return b
			},
		},
	}
}

type harness struct {
	server *httptest.Server
	client *http.Client
}

func proxyNS(name, upstream string, filters ...filter.Spec) *namespace.Namespace {
	ns := integrationtest.ProxyAnonymous(name, upstream)
	ns.Spec.Proxy.Filters = filters
	return ns
}

func newProxyHarness(t *testing.T, b *blob.Bucket, cfg debian.Config, nss ...*namespace.Namespace) *harness {
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
	srv := httptest.NewServer(debian.Handler(reg, auth.AlwaysAnonymous{}, cfg))
	t.Cleanup(srv.Close)
	return &harness{server: srv, client: srv.Client()}
}

func request(t *testing.T, h *harness, method, path string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, h.server.URL+path, body)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("Do %s: %v", method, err)
	}
	return resp
}

func get(t *testing.T, h *harness, path string) *http.Response {
	t.Helper()
	return request(t, h, http.MethodGet, path, nil)
}

func head(t *testing.T, h *harness, path string) *http.Response {
	t.Helper()
	return request(t, h, http.MethodHead, path, nil)
}

func readResp(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	return string(b)
}

const (
	indexRepoPath  = "dists/stable/main/binary-amd64/Packages.gz"
	indexProxyPath = "/team-proxy/debian/dists/stable/main/binary-amd64/Packages.gz"
	debRepoPath    = "pool/main/h/hello/hello_2.10-2_amd64.deb"
	debProxyPath   = "/team-proxy/debian/pool/main/h/hello/hello_2.10-2_amd64.deb"
)

func TestProxyIndexPullThroughAndStaleFallback(t *testing.T) {
	t.Parallel()
	for _, be := range backends() {
		be := be
		t.Run(be.name, func(t *testing.T) {
			t.Parallel()
			body := []byte("Package: hello\nVersion: 2.10-2\n")
			up := newFakeDebian(t)
			up.put(indexRepoPath, body)
			h := newProxyHarness(t, be.open(t), debian.Config{}, proxyNS("team-proxy", up.server.URL))

			// First request fetches upstream and write-throughs the cache.
			first := get(t, h, indexProxyPath)
			if first.StatusCode != http.StatusOK {
				t.Fatalf("first index status = %d: %s", first.StatusCode, readResp(t, first))
			}
			if ct := first.Header.Get("Content-Type"); ct != "application/gzip" {
				t.Fatalf("content type = %q, want application/gzip", ct)
			}
			if got := readResp(t, first); got != string(body) {
				t.Fatalf("index body = %q, want %q", got, body)
			}

			// While upstream is reachable, the index is always pulled fresh.
			second := get(t, h, indexProxyPath)
			_ = readResp(t, second)
			if got := up.hitCount(indexRepoPath); got != 2 {
				t.Fatalf("upstream index hits = %d, want 2 (always fresh when up)", got)
			}

			// Upstream down: the durable cached copy is served as a stale fallback.
			up.setStatus(indexRepoPath, http.StatusInternalServerError)
			stale := get(t, h, indexProxyPath)
			if stale.StatusCode != http.StatusOK {
				t.Fatalf("stale index status = %d, want 200: %s", stale.StatusCode, readResp(t, stale))
			}
			if got := readResp(t, stale); got != string(body) {
				t.Fatalf("stale index body = %q, want %q", got, body)
			}
		})
	}
}

func TestProxyIndexRestartStaleFallback(t *testing.T) {
	t.Parallel()
	bucket := memblob.OpenBucket(nil)
	t.Cleanup(func() { bucket.Close() })
	body := []byte("Release contents")
	up := newFakeDebian(t)
	up.put("dists/stable/Release", body)
	h := newProxyHarness(t, bucket, debian.Config{}, proxyNS("team-proxy", up.server.URL))

	if resp := get(t, h, "/team-proxy/debian/dists/stable/Release"); resp.StatusCode != http.StatusOK {
		t.Fatalf("warm index status = %d: %s", resp.StatusCode, readResp(t, resp))
	} else {
		_ = readResp(t, resp)
	}

	// A fresh server over the same bucket serves the cached index with upstream down.
	up.setStatus("dists/stable/Release", http.StatusInternalServerError)
	h2 := newProxyHarness(t, bucket, debian.Config{}, proxyNS("team-proxy", up.server.URL))
	resp := get(t, h2, "/team-proxy/debian/dists/stable/Release")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post-restart index status = %d, want 200: %s", resp.StatusCode, readResp(t, resp))
	}
	if got := readResp(t, resp); got != string(body) {
		t.Fatalf("post-restart index body = %q", got)
	}
}

func TestProxyIndexUpstreamUnavailableNoCache(t *testing.T) {
	t.Parallel()
	up := newFakeDebian(t)
	up.setStatus("dists/stable/Release", http.StatusInternalServerError)
	h := newProxyHarness(t, memblob.OpenBucket(nil), debian.Config{}, proxyNS("team-proxy", up.server.URL))

	resp := get(t, h, "/team-proxy/debian/dists/stable/Release")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("index 5xx status = %d, want 503: %s", resp.StatusCode, readResp(t, resp))
	}
	_ = readResp(t, resp)
}

func TestProxyIndexUpstream404Negcached(t *testing.T) {
	t.Parallel()
	up := newFakeDebian(t)
	h := newProxyHarness(t, memblob.OpenBucket(nil), debian.Config{}, proxyNS("team-proxy", up.server.URL))

	for i := 0; i < 3; i++ {
		resp := get(t, h, "/team-proxy/debian/dists/stable/Missing")
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("missing index status = %d, want 404: %s", resp.StatusCode, readResp(t, resp))
		}
		_ = readResp(t, resp)
	}
	if got := up.hitCount("dists/stable/Missing"); got != 1 {
		t.Fatalf("upstream hits = %d, want 1 (negative cache absorbs the rest)", got)
	}
}

func TestProxyArtifactMissFillAndRestart(t *testing.T) {
	t.Parallel()
	for _, be := range backends() {
		be := be
		t.Run(be.name, func(t *testing.T) {
			t.Parallel()
			bucket := be.open(t)
			deb := []byte("deb bytes from upstream")
			up := newFakeDebian(t)
			up.put(debRepoPath, deb)
			h := newProxyHarness(t, bucket, debian.Config{}, proxyNS("team-proxy", up.server.URL))

			cold := get(t, h, debProxyPath)
			if cold.StatusCode != http.StatusOK {
				t.Fatalf("cold deb status = %d: %s", cold.StatusCode, readResp(t, cold))
			}
			if ct := cold.Header.Get("Content-Type"); ct != "application/vnd.debian.binary-package" {
				t.Fatalf("content type = %q", ct)
			}
			if got := readResp(t, cold); got != string(deb) {
				t.Fatalf("cold body = %q, want %q", got, deb)
			}
			if got := up.hitCount(debRepoPath); got != 1 {
				t.Fatalf("upstream hits after cold = %d, want 1", got)
			}

			// Upstream goes away; a warm GET is served from the blob cache.
			up.setStatus(debRepoPath, http.StatusInternalServerError)
			warm := get(t, h, debProxyPath)
			if warm.StatusCode != http.StatusOK {
				t.Fatalf("warm deb status = %d: %s", warm.StatusCode, readResp(t, warm))
			}
			if got := readResp(t, warm); got != string(deb) {
				t.Fatalf("warm body = %q, want %q", got, deb)
			}
			if got := up.hitCount(debRepoPath); got != 1 {
				t.Fatalf("upstream hits after warm = %d, want 1 (served from cache)", got)
			}

			// Restart over the same bucket still serves the cached artifact.
			h2 := newProxyHarness(t, bucket, debian.Config{}, proxyNS("team-proxy", up.server.URL))
			restarted := get(t, h2, debProxyPath)
			if restarted.StatusCode != http.StatusOK {
				t.Fatalf("post-restart deb status = %d: %s", restarted.StatusCode, readResp(t, restarted))
			}
			if got := readResp(t, restarted); got != string(deb) {
				t.Fatalf("post-restart deb body = %q", got)
			}
		})
	}
}

func TestProxyArtifactFilterDeny(t *testing.T) {
	t.Parallel()
	up := newFakeDebian(t)
	up.put(debRepoPath, []byte("denied deb"))
	deny := filter.Spec{Kind: filter.KindDeny, Patterns: []string{"hello"}}
	h := newProxyHarness(t, memblob.OpenBucket(nil), debian.Config{}, proxyNS("team-proxy", up.server.URL, deny))

	resp := get(t, h, debProxyPath)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("denied deb status = %d, want 404: %s", resp.StatusCode, readResp(t, resp))
	}
	_ = readResp(t, resp)
	if got := up.hitCount(debRepoPath); got != 0 {
		t.Fatalf("upstream hits for denied artifact = %d, want 0", got)
	}
}

func TestProxyArtifactUpstream404(t *testing.T) {
	t.Parallel()
	up := newFakeDebian(t)
	h := newProxyHarness(t, memblob.OpenBucket(nil), debian.Config{}, proxyNS("team-proxy", up.server.URL))

	resp := get(t, h, debProxyPath)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing deb status = %d, want 404: %s", resp.StatusCode, readResp(t, resp))
	}
	_ = readResp(t, resp)
}

func TestProxyArtifactUpstream500(t *testing.T) {
	t.Parallel()
	up := newFakeDebian(t)
	up.setStatus(debRepoPath, http.StatusInternalServerError)
	h := newProxyHarness(t, memblob.OpenBucket(nil), debian.Config{}, proxyNS("team-proxy", up.server.URL))

	resp := get(t, h, debProxyPath)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("deb 500 status = %d, want 502: %s", resp.StatusCode, readResp(t, resp))
	}
	_ = readResp(t, resp)
}

func TestProxyArtifactNegativeCache(t *testing.T) {
	t.Parallel()
	up := newFakeDebian(t)
	h := newProxyHarness(t, memblob.OpenBucket(nil), debian.Config{}, proxyNS("team-proxy", up.server.URL))

	for i := 0; i < 3; i++ {
		resp := get(t, h, debProxyPath)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("miss %d status = %d, want 404: %s", i, resp.StatusCode, readResp(t, resp))
		}
		_ = readResp(t, resp)
	}
	if got := up.hitCount(debRepoPath); got != 1 {
		t.Fatalf("upstream hits = %d, want 1 (negative cache absorbs the rest)", got)
	}
}

func TestProxyArtifactHead(t *testing.T) {
	t.Parallel()
	up := newFakeDebian(t)
	up.put(debRepoPath, []byte("deb bytes"))
	h := newProxyHarness(t, memblob.OpenBucket(nil), debian.Config{}, proxyNS("team-proxy", up.server.URL))

	resp := head(t, h, debProxyPath)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD status = %d, want 200: %s", resp.StatusCode, readResp(t, resp))
	}
	_ = readResp(t, resp)
	// HEAD does not cache bytes.
	if got := up.hitCount(debRepoPath); got != 1 {
		t.Fatalf("upstream HEAD hits = %d, want 1", got)
	}
}

func TestProxyWriteRoutesReturn405(t *testing.T) {
	t.Parallel()
	up := newFakeDebian(t)
	h := newProxyHarness(t, memblob.OpenBucket(nil), debian.Config{}, proxyNS("team-proxy", up.server.URL))

	for _, method := range []string{http.MethodPut, http.MethodPost, http.MethodDelete} {
		resp := request(t, h, method, debProxyPath, strings.NewReader("bytes"))
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("%s status = %d, want 405: %s", method, resp.StatusCode, readResp(t, resp))
		}
		if allow := resp.Header.Get("Allow"); !strings.Contains(allow, http.MethodGet) {
			t.Fatalf("%s Allow header = %q, want it to list GET", method, allow)
		}
		_ = readResp(t, resp)
	}
}

func TestHostedNamespaceServesNoUpstream(t *testing.T) {
	t.Parallel()
	up := newFakeDebian(t)
	up.put(debRepoPath, []byte("deb bytes"))
	// A hosted Debian namespace never reaches upstream; an empty bucket 404s.
	h := newProxyHarness(t, memblob.OpenBucket(nil), debian.Config{}, integrationtest.HostedAnonymous("team-hosted"))

	resp := get(t, h, "/team-hosted/debian/pool/main/h/hello/hello_2.10-2_amd64.deb")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("hosted deb status = %d, want 404: %s", resp.StatusCode, readResp(t, resp))
	}
	_ = readResp(t, resp)
	if got := up.hitCount(debRepoPath); got != 0 {
		t.Fatalf("hosted namespace contacted upstream %d times, want 0", got)
	}
}
