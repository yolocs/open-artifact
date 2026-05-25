package npm_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"gocloud.dev/blob"
	"gocloud.dev/blob/memblob"

	"github.com/yolocs/open-artifact/pkg/auth"
	"github.com/yolocs/open-artifact/pkg/core"
	"github.com/yolocs/open-artifact/pkg/namespace"
	"github.com/yolocs/open-artifact/pkg/proxy/filter"
	"github.com/yolocs/open-artifact/pkg/surface/integrationtest"
	"github.com/yolocs/open-artifact/pkg/surface/npm"
)

// upstreamVersion is one version published by the fake upstream registry.
type upstreamVersion struct {
	version    string
	tarball    []byte
	uploadTime string // RFC3339; surfaced via packument time[version]
}

// fakeRegistry is a configurable stand-in for an npm-compatible upstream. It
// serves a package's packument and the tarball bytes under /-/, mirroring the
// real registry's route shapes (including the %2f-encoded scoped packument
// path). Tests mutate its fields under the lock to drive 404/500 and to assert
// hit counts.
type fakeRegistry struct {
	server  *httptest.Server
	pkgName string // npm name, e.g. "left-pad" or "@scope/pkg"

	mu                sync.Mutex
	versions          []upstreamVersion
	distTags          map[string]string
	packumentStatus   int            // 0 => 200
	tarballStatus     map[string]int // filename => override status
	packumentHits     int
	tarballHits       int
	lastPackumentPath string
}

func newFakeRegistry(t *testing.T, pkgName string, versions []upstreamVersion, distTags map[string]string) *fakeRegistry {
	t.Helper()
	f := &fakeRegistry{
		pkgName:       pkgName,
		versions:      versions,
		distTags:      distTags,
		tarballStatus: map[string]int{},
	}
	f.server = httptest.NewServer(f)
	t.Cleanup(f.server.Close)
	return f
}

// unscoped returns the name portion without the scope, used for tarball
// filenames ("<unscoped>-<version>.tgz").
func (f *fakeRegistry) unscoped() string {
	if i := strings.IndexByte(f.pkgName, '/'); i >= 0 {
		return f.pkgName[i+1:]
	}
	return f.pkgName
}

func (f *fakeRegistry) tarballFile(version string) string {
	return f.unscoped() + "-" + version + ".tgz"
}

// packumentPath is the request path the proxy must use to fetch this package's
// packument: "/name" unscoped, "/@scope%2fname" scoped.
func (f *fakeRegistry) packumentPath() string {
	if strings.HasPrefix(f.pkgName, "@") {
		rest := f.pkgName[1:]
		slash := strings.IndexByte(rest, '/')
		return "/@" + rest[:slash] + "%2f" + rest[slash+1:]
	}
	return "/" + f.pkgName
}

func (f *fakeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	// Tarball: <base>/<pkgname>/-/<filename>. The pkgname segment may be %2f
	// encoded for scoped packages; we only need the trailing "/-/<file>".
	if i := strings.Index(r.URL.EscapedPath(), "/-/"); i >= 0 {
		f.tarballHits++
		filename := strings.TrimPrefix(r.URL.EscapedPath()[i:], "/-/")
		if st := f.tarballStatus[filename]; st != 0 {
			w.WriteHeader(st)
			return
		}
		for _, v := range f.versions {
			if f.tarballFile(v.version) == filename {
				_, _ = w.Write(v.tarball)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// Packument: everything else is treated as a packument request. Compare the
	// raw (escaped) path so the %2f-encoded scoped form is matched exactly.
	f.packumentHits++
	f.lastPackumentPath = r.URL.EscapedPath()
	if f.packumentStatus != 0 {
		w.WriteHeader(f.packumentStatus)
		return
	}
	if r.URL.EscapedPath() != f.packumentPath() {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(f.packumentJSON())
}

// packumentJSON renders an npm packument with each version's dist.tarball
// pointing at this fake upstream (so the proxy must rewrite it on the way out).
func (f *fakeRegistry) packumentJSON() []byte {
	versions := map[string]any{}
	times := map[string]string{}
	for _, v := range f.versions {
		versions[v.version] = map[string]any{
			"name":    f.pkgName,
			"version": v.version,
			"dist": map[string]any{
				"shasum":  sha1Hex(v.tarball),
				"tarball": f.server.URL + "/" + f.pkgName + "/-/" + f.tarballFile(v.version),
			},
		}
		if v.uploadTime != "" {
			times[v.version] = v.uploadTime
		}
	}
	doc := map[string]any{
		"_id":      f.pkgName,
		"name":     f.pkgName,
		"versions": versions,
	}
	if len(f.distTags) > 0 {
		doc["dist-tags"] = f.distTags
	}
	if len(times) > 0 {
		doc["time"] = times
	}
	b, _ := json.Marshal(doc)
	return b
}

func (f *fakeRegistry) setPackumentStatus(code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.packumentStatus = code
}

func (f *fakeRegistry) setTarballStatus(filename string, code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tarballStatus[filename] = code
}

func (f *fakeRegistry) packumentRequests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.packumentHits
}

func (f *fakeRegistry) lastPackument() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastPackumentPath
}

// TestProxyCacheSurvivesRestart proves the durable packument snapshot and the
// pulled-through tarball File persist in the bucket across a process restart: a
// fresh Registry/Handler over the same bucket (with empty in-process caches and
// upstream down) still serves both. It runs against mem:// and file://.
func TestProxyCacheSurvivesRestart(t *testing.T) {
	t.Parallel()

	for _, be := range backends() {
		be := be
		t.Run(be.name, func(t *testing.T) {
			t.Parallel()
			b := be.open(t)
			tarball := []byte("durable tarball bytes")
			up := newFakeRegistry(t, "left-pad",
				[]upstreamVersion{{version: "1.0.0", tarball: tarball}}, map[string]string{"latest": "1.0.0"})

			// First "process": a cold tarball download fetches the packument and
			// fills both the durable packument snapshot and the tarball File.
			h1 := newProxyHarness(t, b, npm.Config{ProxyPackumentMemoTTL: -1},
				proxyNamespace("team-proxy", up.server.URL))
			if dl := do(t, h1, http.MethodGet, "/team-proxy"+tarballPath("left-pad", "1.0.0")); dl.StatusCode != http.StatusOK {
				t.Fatalf("cold download = %d: %s", dl.StatusCode, readResp(t, dl))
			} else {
				_ = readResp(t, dl)
			}

			// Upstream goes away.
			up.setPackumentStatus(http.StatusInternalServerError)
			up.setTarballStatus(up.tarballFile("1.0.0"), http.StatusInternalServerError)

			// Second "process": a fresh Registry/Handler over the SAME bucket has
			// no in-process memo or negative cache, yet serves from the bucket.
			h2 := newProxyHarness(t, b, npm.Config{ProxyPackumentMemoTTL: -1},
				proxyNamespace("team-proxy", up.server.URL))

			pkmt := decodeJSON(t, do(t, h2, http.MethodGet, "/team-proxy/left-pad"))
			versions, _ := pkmt["versions"].(map[string]any)
			if _, ok := versions["1.0.0"]; !ok {
				t.Fatalf("packument did not survive restart: %v", pkmt)
			}

			dl2 := do(t, h2, http.MethodGet, "/team-proxy"+tarballPath("left-pad", "1.0.0"))
			if dl2.StatusCode != http.StatusOK {
				t.Fatalf("tarball did not survive restart: %d: %s", dl2.StatusCode, readResp(t, dl2))
			}
			if got := readResp(t, dl2); got != string(tarball) {
				t.Fatalf("restored tarball body = %q, want %q", got, tarball)
			}
		})
	}
}

// proxyNamespace builds a proxy npm namespace with optional filters.
func proxyNamespace(name, upstream string, filters ...filter.Spec) *namespace.Namespace {
	ns := integrationtest.ProxyAnonymous(name, upstream)
	ns.Spec.Proxy.Filters = filters
	return ns
}

func newProxyHarness(t *testing.T, b *blob.Bucket, cfg npm.Config, nss ...*namespace.Namespace) *harness {
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
	srv := httptest.NewServer(npm.Handler(reg, auth.AlwaysAnonymous{}, cfg))
	t.Cleanup(srv.Close)
	return &harness{server: srv, reg: reg}
}

func TestProxyPackumentAndTarballFill(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		pkg  string
	}{
		{name: "unscoped", pkg: "left-pad"},
		{name: "scoped", pkg: "@scope/pkg"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tarball := []byte("tarball bytes for " + tc.pkg)
			up := newFakeRegistry(t, tc.pkg, []upstreamVersion{{version: "1.0.0", tarball: tarball}}, map[string]string{"latest": "1.0.0"})
			h := newProxyHarness(t, memblob.OpenBucket(nil), npm.Config{ProxyPackumentMemoTTL: -1},
				proxyNamespace("team-proxy", up.server.URL))

			// Packument: dist-tags preserved, tarball URL rewritten to open-artifact.
			pkmt := decodeJSON(t, do(t, h, http.MethodGet, "/team-proxy/"+urlName(tc.pkg)))
			if pkmt["name"] != tc.pkg {
				t.Fatalf("packument name = %v, want %q", pkmt["name"], tc.pkg)
			}
			distTags, _ := pkmt["dist-tags"].(map[string]any)
			if distTags["latest"] != "1.0.0" {
				t.Fatalf("dist-tags.latest = %v, want 1.0.0", distTags["latest"])
			}
			versions, _ := pkmt["versions"].(map[string]any)
			v1, _ := versions["1.0.0"].(map[string]any)
			dist, _ := v1["dist"].(map[string]any)
			wantTarball := h.server.URL + "/team-proxy" + tarballPath(tc.pkg, "1.0.0")
			if dist["tarball"] != wantTarball {
				t.Fatalf("tarball URL = %v, want %q (must point back at open-artifact)", dist["tarball"], wantTarball)
			}

			// Cold tarball download fetches upstream, fills the cache, serves bytes.
			dl := do(t, h, http.MethodGet, "/team-proxy"+tarballPath(tc.pkg, "1.0.0"))
			if dl.StatusCode != http.StatusOK {
				t.Fatalf("download status = %d: %s", dl.StatusCode, readResp(t, dl))
			}
			if got := readResp(t, dl); got != string(tarball) {
				t.Fatalf("download body = %q, want %q", got, tarball)
			}

			// Second install can succeed from the blob cache with upstream down.
			up.setPackumentStatus(http.StatusInternalServerError)
			up.setTarballStatus(up.tarballFile("1.0.0"), http.StatusInternalServerError)
			again := do(t, h, http.MethodGet, "/team-proxy"+tarballPath(tc.pkg, "1.0.0"))
			if again.StatusCode != http.StatusOK {
				t.Fatalf("cached download status = %d: %s", again.StatusCode, readResp(t, again))
			}
			if got := readResp(t, again); got != string(tarball) {
				t.Fatalf("cached download body = %q, want %q", got, tarball)
			}
		})
	}
}

func TestProxyScopedPackumentPathEncoding(t *testing.T) {
	t.Parallel()

	up := newFakeRegistry(t, "@scope/pkg", []upstreamVersion{{version: "1.0.0", tarball: []byte("x")}}, nil)
	h := newProxyHarness(t, memblob.OpenBucket(nil), npm.Config{ProxyPackumentMemoTTL: -1},
		proxyNamespace("team-proxy", up.server.URL))

	resp := do(t, h, http.MethodGet, "/team-proxy/@scope%2fpkg")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scoped packument status = %d: %s", resp.StatusCode, readResp(t, resp))
	}
	_ = readResp(t, resp)
	if got := up.lastPackument(); got != "/@scope%2fpkg" {
		t.Fatalf("upstream packument path = %q, want /@scope%%2fpkg", got)
	}
}

func TestProxyPackumentMemoAbsorbsBurst(t *testing.T) {
	t.Parallel()

	up := newFakeRegistry(t, "left-pad", []upstreamVersion{{version: "1.0.0", tarball: []byte("x")}}, nil)
	// A long memo TTL so repeated reads hit the in-process cache, not upstream.
	h := newProxyHarness(t, memblob.OpenBucket(nil), npm.Config{},
		proxyNamespace("team-proxy", up.server.URL))

	for i := 0; i < 3; i++ {
		resp := do(t, h, http.MethodGet, "/team-proxy/left-pad")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("packument status = %d: %s", resp.StatusCode, readResp(t, resp))
		}
		_ = readResp(t, resp)
	}
	if hits := up.packumentRequests(); hits != 1 {
		t.Fatalf("upstream packument hits = %d, want 1 (memo should absorb the rest)", hits)
	}
}

func TestProxyDurableCacheServesFreshWithoutUpstream(t *testing.T) {
	t.Parallel()

	up := newFakeRegistry(t, "left-pad", []upstreamVersion{{version: "1.0.0", tarball: []byte("x")}}, nil)
	// Memo disabled, but the durable cache TTL is the default (10m), so the
	// second read serves from the durable snapshot without contacting upstream.
	h := newProxyHarness(t, memblob.OpenBucket(nil), npm.Config{ProxyPackumentMemoTTL: -1},
		proxyNamespace("team-proxy", up.server.URL))

	if resp := do(t, h, http.MethodGet, "/team-proxy/left-pad"); resp.StatusCode != http.StatusOK {
		t.Fatalf("first packument = %d: %s", resp.StatusCode, readResp(t, resp))
	} else {
		_ = readResp(t, resp)
	}
	if resp := do(t, h, http.MethodGet, "/team-proxy/left-pad"); resp.StatusCode != http.StatusOK {
		t.Fatalf("second packument = %d: %s", resp.StatusCode, readResp(t, resp))
	} else {
		_ = readResp(t, resp)
	}
	if hits := up.packumentRequests(); hits != 1 {
		t.Fatalf("upstream packument hits = %d, want 1 (durable cache should serve the second read)", hits)
	}
}

func TestProxyStalePackumentFallback(t *testing.T) {
	t.Parallel()

	up := newFakeRegistry(t, "left-pad", []upstreamVersion{{version: "1.0.0", tarball: []byte("x")}}, map[string]string{"latest": "1.0.0"})
	// Negative durable TTL: the snapshot is never served as fresh, so every read
	// goes upstream and falls back to the stale snapshot when upstream fails.
	h := newProxyHarness(t, memblob.OpenBucket(nil), npm.Config{ProxyPackumentMemoTTL: -1, ProxyPackumentCacheTTL: -1},
		proxyNamespace("team-proxy", up.server.URL))

	if resp := do(t, h, http.MethodGet, "/team-proxy/left-pad"); resp.StatusCode != http.StatusOK {
		t.Fatalf("first packument = %d: %s", resp.StatusCode, readResp(t, resp))
	} else {
		_ = readResp(t, resp)
	}

	up.setPackumentStatus(http.StatusInternalServerError)
	stale := do(t, h, http.MethodGet, "/team-proxy/left-pad")
	if stale.StatusCode != http.StatusOK {
		t.Fatalf("stale packument = %d, want 200: %s", stale.StatusCode, readResp(t, stale))
	}
	pkmt := decodeJSON(t, stale)
	versions, _ := pkmt["versions"].(map[string]any)
	if _, ok := versions["1.0.0"]; !ok {
		t.Fatalf("stale packument missing cached version: %v", pkmt)
	}
}

func TestProxySynthesizedPackumentFallback(t *testing.T) {
	t.Parallel()

	tarball := []byte("the tarball")
	up := newFakeRegistry(t, "left-pad", []upstreamVersion{{version: "1.0.0", tarball: tarball}}, map[string]string{"latest": "1.0.0"})
	h := newProxyHarness(t, memblob.OpenBucket(nil), npm.Config{ProxyPackumentMemoTTL: -1, ProxyPackumentCacheTTL: -1},
		proxyNamespace("team-proxy", up.server.URL))

	// Warm the tarball + package.json caches via a normal download.
	if dl := do(t, h, http.MethodGet, "/team-proxy"+tarballPath("left-pad", "1.0.0")); dl.StatusCode != http.StatusOK {
		t.Fatalf("warm download = %d: %s", dl.StatusCode, readResp(t, dl))
	} else {
		_ = readResp(t, dl)
	}

	// Evict the durable packument snapshot, then take upstream down: the
	// packument must be synthesized from the locally cached version metadata.
	scoped, err := h.reg.For("team-proxy", core.FormatNPM.String())
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	store, err := scoped.Store()
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if err := store.Cache("npm:packument:u/left-pad").Delete(t.Context()); err != nil {
		t.Fatalf("evict packument cache: %v", err)
	}
	up.setPackumentStatus(http.StatusInternalServerError)

	syn := do(t, h, http.MethodGet, "/team-proxy/left-pad")
	if syn.StatusCode != http.StatusOK {
		t.Fatalf("synthesized packument = %d, want 200: %s", syn.StatusCode, readResp(t, syn))
	}
	pkmt := decodeJSON(t, syn)
	versions, _ := pkmt["versions"].(map[string]any)
	v1, _ := versions["1.0.0"].(map[string]any)
	dist, _ := v1["dist"].(map[string]any)
	wantTarball := h.server.URL + "/team-proxy" + tarballPath("left-pad", "1.0.0")
	if dist["tarball"] != wantTarball {
		t.Fatalf("synthesized tarball URL = %v, want %q", dist["tarball"], wantTarball)
	}
}

func TestProxyPackumentUpstream404(t *testing.T) {
	t.Parallel()

	up := newFakeRegistry(t, "left-pad", nil, nil)
	up.setPackumentStatus(http.StatusNotFound)
	h := newProxyHarness(t, memblob.OpenBucket(nil), npm.Config{ProxyPackumentMemoTTL: -1},
		proxyNamespace("team-proxy", up.server.URL))

	for i := 0; i < 3; i++ {
		resp := do(t, h, http.MethodGet, "/team-proxy/left-pad")
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: %s", resp.StatusCode, readResp(t, resp))
		}
		_ = readResp(t, resp)
	}
	if hits := up.packumentRequests(); hits != 1 {
		t.Fatalf("upstream hits = %d, want 1 (negative cache should absorb the rest)", hits)
	}
}

func TestProxyPackumentUpstream500NoCacheReturns503(t *testing.T) {
	t.Parallel()

	up := newFakeRegistry(t, "left-pad", nil, nil)
	up.setPackumentStatus(http.StatusInternalServerError)
	h := newProxyHarness(t, memblob.OpenBucket(nil), npm.Config{ProxyPackumentMemoTTL: -1},
		proxyNamespace("team-proxy", up.server.URL))

	resp := do(t, h, http.MethodGet, "/team-proxy/left-pad")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", resp.StatusCode, readResp(t, resp))
	}
	_ = readResp(t, resp)
}

func TestProxyTarballMissReturns404(t *testing.T) {
	t.Parallel()

	up := newFakeRegistry(t, "left-pad", []upstreamVersion{{version: "1.0.0", tarball: []byte("x")}}, nil)
	h := newProxyHarness(t, memblob.OpenBucket(nil), npm.Config{ProxyPackumentMemoTTL: -1},
		proxyNamespace("team-proxy", up.server.URL))

	resp := do(t, h, http.MethodGet, "/team-proxy"+tarballPath("left-pad", "9.9.9"))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", resp.StatusCode, readResp(t, resp))
	}
	_ = readResp(t, resp)
}

func TestProxyTarballHeadDoesNotCache(t *testing.T) {
	t.Parallel()

	tarball := []byte("tarball bytes")
	up := newFakeRegistry(t, "left-pad", []upstreamVersion{{version: "1.0.0", tarball: tarball}}, nil)
	h := newProxyHarness(t, memblob.OpenBucket(nil), npm.Config{ProxyPackumentMemoTTL: -1},
		proxyNamespace("team-proxy", up.server.URL))

	head := do(t, h, http.MethodHead, "/team-proxy"+tarballPath("left-pad", "1.0.0"))
	if head.StatusCode != http.StatusOK {
		t.Fatalf("HEAD status = %d: %s", head.StatusCode, readResp(t, head))
	}
	if b := readResp(t, head); b != "" {
		t.Fatalf("HEAD body = %q, want empty", b)
	}

	// HEAD must not have cached bytes: with upstream now refusing the tarball, a
	// GET fails rather than serving a cached copy.
	up.setTarballStatus(up.tarballFile("1.0.0"), http.StatusInternalServerError)
	dl := do(t, h, http.MethodGet, "/team-proxy"+tarballPath("left-pad", "1.0.0"))
	if dl.StatusCode == http.StatusOK {
		t.Fatalf("GET after HEAD = 200, want failure (HEAD must not cache bytes)")
	}
	_ = readResp(t, dl)
}

func TestProxyFilterDeny(t *testing.T) {
	t.Parallel()

	up := newFakeRegistry(t, "left-pad", []upstreamVersion{{version: "1.0.0", tarball: []byte("x")}}, map[string]string{"latest": "1.0.0"})
	h := newProxyHarness(t, memblob.OpenBucket(nil), npm.Config{ProxyPackumentMemoTTL: -1},
		proxyNamespace("team-proxy", up.server.URL, filter.Spec{Kind: filter.KindDeny, Patterns: []string{"left-pad"}}))

	// The packument still renders (filters apply only to tarball downloads).
	if pkmt := do(t, h, http.MethodGet, "/team-proxy/left-pad"); pkmt.StatusCode != http.StatusOK {
		t.Fatalf("packument status = %d: %s", pkmt.StatusCode, readResp(t, pkmt))
	} else {
		_ = readResp(t, pkmt)
	}
	dl := do(t, h, http.MethodGet, "/team-proxy"+tarballPath("left-pad", "1.0.0"))
	if dl.StatusCode != http.StatusNotFound {
		t.Fatalf("denied download = %d, want 404: %s", dl.StatusCode, readResp(t, dl))
	}
	_ = readResp(t, dl)
}

func TestProxyDelayFilter(t *testing.T) {
	t.Parallel()

	t.Run("recent_upload_denied", func(t *testing.T) {
		t.Parallel()
		up := newFakeRegistry(t, "left-pad",
			[]upstreamVersion{{version: "1.0.0", tarball: []byte("x"), uploadTime: "2999-01-01T00:00:00Z"}}, nil)
		h := newProxyHarness(t, memblob.OpenBucket(nil), npm.Config{ProxyPackumentMemoTTL: -1},
			proxyNamespace("team-proxy", up.server.URL, filter.Spec{Kind: filter.KindDelay, MinAge: "24h"}))

		dl := do(t, h, http.MethodGet, "/team-proxy"+tarballPath("left-pad", "1.0.0"))
		if dl.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (too new): %s", dl.StatusCode, readResp(t, dl))
		}
		_ = readResp(t, dl)
	})

	t.Run("old_upload_allowed", func(t *testing.T) {
		t.Parallel()
		up := newFakeRegistry(t, "left-pad",
			[]upstreamVersion{{version: "1.0.0", tarball: []byte("payload"), uploadTime: "2000-01-01T00:00:00Z"}}, nil)
		h := newProxyHarness(t, memblob.OpenBucket(nil), npm.Config{ProxyPackumentMemoTTL: -1},
			proxyNamespace("team-proxy", up.server.URL, filter.Spec{Kind: filter.KindDelay, MinAge: "24h"}))

		dl := do(t, h, http.MethodGet, "/team-proxy"+tarballPath("left-pad", "1.0.0"))
		if dl.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 (old enough): %s", dl.StatusCode, readResp(t, dl))
		}
		_ = readResp(t, dl)
	})

	t.Run("missing_upload_time_fails_closed", func(t *testing.T) {
		t.Parallel()
		// No upload time in the packument: a delay filter cannot resolve the
		// publish time and must fail closed.
		up := newFakeRegistry(t, "left-pad",
			[]upstreamVersion{{version: "1.0.0", tarball: []byte("x")}}, nil)
		h := newProxyHarness(t, memblob.OpenBucket(nil), npm.Config{ProxyPackumentMemoTTL: -1},
			proxyNamespace("team-proxy", up.server.URL, filter.Spec{Kind: filter.KindDelay, MinAge: "24h"}))

		dl := do(t, h, http.MethodGet, "/team-proxy"+tarballPath("left-pad", "1.0.0"))
		if dl.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (fail closed): %s", dl.StatusCode, readResp(t, dl))
		}
		_ = readResp(t, dl)
	})
}

func TestProxyDenyAllPolicyForbids(t *testing.T) {
	t.Parallel()

	up := newFakeRegistry(t, "left-pad", []upstreamVersion{{version: "1.0.0", tarball: []byte("x")}}, nil)
	denyAll := &namespace.Namespace{
		Name: "team-proxy",
		Spec: namespace.Spec{
			Mode:  namespace.ModeProxy,
			Proxy: namespace.Proxy{Upstream: up.server.URL},
		},
	}
	h := newProxyHarness(t, memblob.OpenBucket(nil), npm.Config{ProxyPackumentMemoTTL: -1}, denyAll)

	for _, path := range []string{
		"/team-proxy/left-pad",
		"/team-proxy" + tarballPath("left-pad", "1.0.0"),
	} {
		resp := do(t, h, http.MethodGet, path)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s status = %d, want 403: %s", path, resp.StatusCode, readResp(t, resp))
		}
		_ = readResp(t, resp)
	}
	if hits := up.packumentRequests(); hits != 0 {
		t.Fatalf("upstream hits = %d, want 0 (denied before any upstream fetch)", hits)
	}
}

func TestProxyReaderPolicyPopulatesCache(t *testing.T) {
	t.Parallel()

	tarball := []byte("readable bytes")
	up := newFakeRegistry(t, "left-pad", []upstreamVersion{{version: "1.0.0", tarball: tarball}}, nil)
	// ProxyAnonymous grants only a reader policy (no writers); the cache fill
	// must still succeed because pull-through authorizes as a read.
	h := newProxyHarness(t, memblob.OpenBucket(nil), npm.Config{ProxyPackumentMemoTTL: -1},
		proxyNamespace("team-proxy", up.server.URL))

	dl := do(t, h, http.MethodGet, "/team-proxy"+tarballPath("left-pad", "1.0.0"))
	if dl.StatusCode != http.StatusOK {
		t.Fatalf("download = %d: %s", dl.StatusCode, readResp(t, dl))
	}
	_ = readResp(t, dl)

	// The tarball is now a real File in the proxy store.
	scoped, err := h.reg.For("team-proxy", core.FormatNPM.String())
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	store, err := scoped.Store()
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	exists, err := store.Package("u/left-pad").Version("1.0.0").File("left-pad-1.0.0.tgz").Exists(t.Context())
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if !exists {
		t.Fatalf("tarball was not cached as a File under a reader-only policy")
	}
}
