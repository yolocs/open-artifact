package pypi_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
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
	"github.com/yolocs/open-artifact/pkg/surface/pypi"
)

type upstreamFile struct {
	filename       string
	body           []byte
	requiresPython string
	uploadTime     string // RFC3339; surfaced via per-release JSON
}

func (f upstreamFile) sha() string {
	sum := sha256.Sum256(f.body)
	return hex.EncodeToString(sum[:])
}

// fakeUpstream is a configurable stand-in for a PyPI-compatible upstream. It
// serves a project's simple index (HTML or PEP 691 JSON), the file bytes under
// /files/, and per-release JSON under /pypi/. Tests mutate its fields under the
// lock to drive 404/500 and to assert hit counts.
type fakeUpstream struct {
	server  *httptest.Server
	project string

	mu           sync.Mutex
	files        []upstreamFile
	useJSON      bool
	simpleStatus int            // 0 => 200
	fileStatus   map[string]int // filename => override status
	releaseJSON  map[string]string
	simpleHits   int
}

func newFakeUpstream(t *testing.T, project string, files []upstreamFile, useJSON bool) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{
		project:     project,
		files:       files,
		useJSON:     useJSON,
		fileStatus:  map[string]int{},
		releaseJSON: map[string]string{},
	}
	f.server = httptest.NewServer(f)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.URL.Path == "/simple/"+f.project+"/":
		f.simpleHits++
		if f.simpleStatus != 0 {
			w.WriteHeader(f.simpleStatus)
			return
		}
		if f.useJSON {
			w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
			_, _ = w.Write(jsonIndex(f.project, f.files))
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write(htmlIndex(f.files))
	case strings.HasPrefix(r.URL.Path, "/files/"):
		name := strings.TrimPrefix(r.URL.Path, "/files/")
		if st := f.fileStatus[name]; st != 0 {
			w.WriteHeader(st)
			return
		}
		for _, uf := range f.files {
			if uf.filename == name {
				_, _ = w.Write(uf.body)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	case strings.HasPrefix(r.URL.Path, "/pypi/"):
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/") // pypi/<project>/<version>/json
		if len(parts) == 4 && parts[3] == "json" {
			if body, ok := f.releaseJSON[parts[2]]; ok {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeUpstream) setSimpleStatus(code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.simpleStatus = code
}

func (f *fakeUpstream) hits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.simpleHits
}

// htmlIndex renders a PEP 503 page with relative file links to exercise
// relative-URL resolution against the simple index URL.
func htmlIndex(files []upstreamFile) []byte {
	var b strings.Builder
	b.WriteString("<!DOCTYPE html><html><body>\n")
	for _, f := range files {
		href := "../../files/" + f.filename + "#sha256=" + f.sha()
		b.WriteString(`<a href="` + href + `"`)
		if f.requiresPython != "" {
			b.WriteString(` data-requires-python="` + html.EscapeString(f.requiresPython) + `"`)
		}
		b.WriteString(`>` + f.filename + "</a>\n")
	}
	b.WriteString("</body></html>")
	return []byte(b.String())
}

// jsonIndex renders a PEP 691 (api-version 1.1) document with absolute file
// paths and optional upload-time.
func jsonIndex(project string, files []upstreamFile) []byte {
	type fj struct {
		Filename       string            `json:"filename"`
		URL            string            `json:"url"`
		Hashes         map[string]string `json:"hashes"`
		RequiresPython string            `json:"requires-python,omitempty"`
		UploadTime     string            `json:"upload-time,omitempty"`
	}
	out := struct {
		Meta  map[string]string `json:"meta"`
		Name  string            `json:"name"`
		Files []fj              `json:"files"`
	}{
		Meta: map[string]string{"api-version": "1.1"},
		Name: project,
	}
	for _, f := range files {
		out.Files = append(out.Files, fj{
			Filename:       f.filename,
			URL:            "/files/" + f.filename,
			Hashes:         map[string]string{"sha256": f.sha()},
			RequiresPython: f.requiresPython,
			UploadTime:     f.uploadTime,
		})
	}
	b, _ := json.Marshal(out)
	return b
}

func proxyNamespace(name, upstream string, filters ...filter.Spec) *namespace.Namespace {
	ns := integrationtest.ProxyAnonymous(name, upstream)
	ns.Spec.Proxy.Filters = filters
	return ns
}

func newProxyHarness(t *testing.T, b *blob.Bucket, cfg pypi.Config, nss ...*namespace.Namespace) *harness {
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
	srv := httptest.NewServer(pypi.Handler(reg, auth.AlwaysAnonymous{}, cfg))
	t.Cleanup(srv.Close)
	return &harness{server: srv, reg: reg}
}

func TestProxyProjectIndexAndColdFill(t *testing.T) {
	t.Parallel()

	for _, useJSON := range []bool{false, true} {
		useJSON := useJSON
		name := "html"
		if useJSON {
			name = "json"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			wheel := upstreamFile{
				filename:       "demo_pkg-1.2.3-py3-none-any.whl",
				body:           []byte("the wheel bytes"),
				requiresPython: ">=3.8",
				uploadTime:     "2020-01-01T00:00:00Z",
			}
			up := newFakeUpstream(t, "demo-pkg", []upstreamFile{wheel}, useJSON)
			h := newProxyHarness(t, memblob.OpenBucket(nil),
				pypi.Config{ProxyIndexCacheTTL: -1},
				proxyNamespace("team-proxy", up.server.URL))

			// Project index: links are rewritten back through open-artifact and
			// carry the upstream sha256 and requires-python.
			idx := get(t, h, "/team-proxy/simple/demo-pkg/", "")
			if idx.StatusCode != http.StatusOK {
				t.Fatalf("index status = %d: %s", idx.StatusCode, readResp(t, idx))
			}
			body := readResp(t, idx)
			wantURL := "/team-proxy/packages/demo-pkg/1.2.3/" + wheel.filename + "#sha256=" + wheel.sha()
			for _, want := range []string{wantURL, `data-requires-python="&gt;=3.8"`} {
				if !strings.Contains(body, want) {
					t.Fatalf("index missing %q: %s", want, body)
				}
			}

			// JSON negotiation renders the same data as PEP 691.
			js := get(t, h, "/team-proxy/simple/demo-pkg/", "application/vnd.pypi.simple.v1+json")
			if ct := js.Header.Get("Content-Type"); ct != "application/vnd.pypi.simple.v1+json" {
				t.Fatalf("json content-type = %q", ct)
			}
			if jb := readResp(t, js); !strings.Contains(jb, `"sha256":"`+wheel.sha()+`"`) {
				t.Fatalf("json index missing hash: %s", jb)
			}

			// Cold download fetches upstream, fills the cache, and serves bytes.
			dl := get(t, h, "/team-proxy/packages/demo-pkg/1.2.3/"+wheel.filename, "")
			if dl.StatusCode != http.StatusOK {
				t.Fatalf("download status = %d: %s", dl.StatusCode, readResp(t, dl))
			}
			if got := readResp(t, dl); got != string(wheel.body) {
				t.Fatalf("download body = %q, want %q", got, wheel.body)
			}

			// With upstream now refusing the file, the cached copy still serves.
			up.mu.Lock()
			up.fileStatus[wheel.filename] = http.StatusInternalServerError
			up.mu.Unlock()
			again := get(t, h, "/team-proxy/packages/demo-pkg/1.2.3/"+wheel.filename, "")
			if again.StatusCode != http.StatusOK {
				t.Fatalf("cached download status = %d: %s", again.StatusCode, readResp(t, again))
			}
			if got := readResp(t, again); got != string(wheel.body) {
				t.Fatalf("cached download body = %q, want %q", got, wheel.body)
			}
		})
	}
}

func TestProxyUpstreamNotFoundNegativeCached(t *testing.T) {
	t.Parallel()

	up := newFakeUpstream(t, "ghost", nil, false)
	up.setSimpleStatus(http.StatusNotFound)
	h := newProxyHarness(t, memblob.OpenBucket(nil),
		pypi.Config{ProxyIndexCacheTTL: -1},
		proxyNamespace("team-proxy", up.server.URL))

	for i := 0; i < 3; i++ {
		resp := get(t, h, "/team-proxy/simple/ghost/", "")
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: %s", resp.StatusCode, readResp(t, resp))
		}
		_ = readResp(t, resp)
	}
	if hits := up.hits(); hits != 1 {
		t.Fatalf("upstream hits = %d, want 1 (negative cache should absorb the rest)", hits)
	}
}

func TestProxyUpstreamServerErrorNoCacheReturns503(t *testing.T) {
	t.Parallel()

	up := newFakeUpstream(t, "broken", nil, false)
	up.setSimpleStatus(http.StatusInternalServerError)
	h := newProxyHarness(t, memblob.OpenBucket(nil),
		pypi.Config{ProxyIndexCacheTTL: -1},
		proxyNamespace("team-proxy", up.server.URL))

	resp := get(t, h, "/team-proxy/simple/broken/", "")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", resp.StatusCode, readResp(t, resp))
	}
	_ = readResp(t, resp)
}

func TestProxyStaleIndexFallback(t *testing.T) {
	t.Parallel()

	wheel := upstreamFile{filename: "demo-1.0.0.tar.gz", body: []byte("sdist")}
	up := newFakeUpstream(t, "demo", []upstreamFile{wheel}, false)
	// No in-process cache: every request re-resolves, so the second one hits the
	// (now failing) upstream and must fall back to the durable snapshot.
	h := newProxyHarness(t, memblob.OpenBucket(nil),
		pypi.Config{ProxyIndexCacheTTL: -1},
		proxyNamespace("team-proxy", up.server.URL))

	first := get(t, h, "/team-proxy/simple/demo/", "")
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first index status = %d: %s", first.StatusCode, readResp(t, first))
	}
	_ = readResp(t, first)

	up.setSimpleStatus(http.StatusInternalServerError)
	stale := get(t, h, "/team-proxy/simple/demo/", "")
	if stale.StatusCode != http.StatusOK {
		t.Fatalf("stale index status = %d, want 200: %s", stale.StatusCode, readResp(t, stale))
	}
	if body := readResp(t, stale); !strings.Contains(body, wheel.filename) {
		t.Fatalf("stale index missing cached file: %s", body)
	}
}

func TestProxySynthesizedIndexFallback(t *testing.T) {
	t.Parallel()

	up := newFakeUpstream(t, "demo", nil, false)
	up.setSimpleStatus(http.StatusInternalServerError)
	b := memblob.OpenBucket(nil)
	h := newProxyHarness(t, b,
		pypi.Config{ProxyIndexCacheTTL: -1},
		proxyNamespace("team-proxy", up.server.URL))

	// Seed a file directly into the proxy store (no cached index exists).
	scoped, err := h.reg.For("team-proxy", core.FormatPyPI.String())
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	store, err := scoped.Store()
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if _, err := store.Package("demo").Version("1.0.0").AddFile(t.Context(), "demo-1.0.0-py3-none-any.whl", strings.NewReader("bytes")); err != nil {
		t.Fatalf("AddFile: %v", err)
	}

	resp := get(t, h, "/team-proxy/simple/demo/", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("synth index status = %d, want 200: %s", resp.StatusCode, readResp(t, resp))
	}
	if body := readResp(t, resp); !strings.Contains(body, "demo-1.0.0-py3-none-any.whl") {
		t.Fatalf("synthesized index missing local file: %s", body)
	}
}

func TestProxyDownloadFileMissReturns404(t *testing.T) {
	t.Parallel()

	wheel := upstreamFile{filename: "demo-1.0.0-py3-none-any.whl", body: []byte("x")}
	up := newFakeUpstream(t, "demo", []upstreamFile{wheel}, false)
	h := newProxyHarness(t, memblob.OpenBucket(nil),
		pypi.Config{ProxyIndexCacheTTL: -1},
		proxyNamespace("team-proxy", up.server.URL))

	resp := get(t, h, "/team-proxy/packages/demo/9.9.9/demo-9.9.9-py3-none-any.whl", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", resp.StatusCode, readResp(t, resp))
	}
	_ = readResp(t, resp)
}

func TestProxyUploadReturns405(t *testing.T) {
	t.Parallel()

	up := newFakeUpstream(t, "demo", nil, false)
	h := newProxyHarness(t, memblob.OpenBucket(nil), pypi.Config{},
		proxyNamespace("team-proxy", up.server.URL))

	resp := upload(t, h, "team-proxy", "demo", "1.0.0", "demo-1.0.0-py3-none-any.whl", []byte("x"), nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405: %s", resp.StatusCode, readResp(t, resp))
	}
	if allow := resp.Header.Get("Allow"); allow != "GET, HEAD" {
		t.Fatalf("Allow = %q, want \"GET, HEAD\"", allow)
	}
	_ = readResp(t, resp)
}

func TestProxyFilterDeny(t *testing.T) {
	t.Parallel()

	wheel := upstreamFile{filename: "demo-1.0.0-py3-none-any.whl", body: []byte("x")}
	up := newFakeUpstream(t, "demo", []upstreamFile{wheel}, false)
	h := newProxyHarness(t, memblob.OpenBucket(nil),
		pypi.Config{ProxyIndexCacheTTL: -1},
		proxyNamespace("team-proxy", up.server.URL, filter.Spec{Kind: filter.KindDeny, Patterns: []string{"demo"}}))

	// The index still renders (filters apply only to artifact downloads).
	if idx := get(t, h, "/team-proxy/simple/demo/", ""); idx.StatusCode != http.StatusOK {
		t.Fatalf("index status = %d: %s", idx.StatusCode, readResp(t, idx))
	}
	dl := get(t, h, "/team-proxy/packages/demo/1.0.0/demo-1.0.0-py3-none-any.whl", "")
	if dl.StatusCode != http.StatusNotFound {
		t.Fatalf("denied download status = %d, want 404: %s", dl.StatusCode, readResp(t, dl))
	}
	_ = readResp(t, dl)
}

func TestProxyDenyAllPolicyForbids(t *testing.T) {
	t.Parallel()

	up := newFakeUpstream(t, "demo", []upstreamFile{{filename: "demo-1.0.0-py3-none-any.whl", body: []byte("x")}}, false)
	// A proxy namespace with an empty policy is deny-all: even a pull-through
	// read is gated by the reader policy.
	denyAll := &namespace.Namespace{
		Name: "team-proxy",
		Spec: namespace.Spec{
			Mode:  namespace.ModeProxy,
			Proxy: namespace.Proxy{Upstream: up.server.URL},
		},
	}
	h := newProxyHarness(t, memblob.OpenBucket(nil), pypi.Config{ProxyIndexCacheTTL: -1}, denyAll)

	for _, path := range []string{
		"/team-proxy/simple/demo/",
		"/team-proxy/packages/demo/1.0.0/demo-1.0.0-py3-none-any.whl",
	} {
		resp := get(t, h, path, "")
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s status = %d, want 403: %s", path, resp.StatusCode, readResp(t, resp))
		}
		_ = readResp(t, resp)
	}
	if hits := up.hits(); hits != 0 {
		t.Fatalf("upstream hits = %d, want 0 (denied before any upstream fetch)", hits)
	}
}

func TestProxyDelayFilter(t *testing.T) {
	t.Parallel()

	filename := "demo-1.0.0-py3-none-any.whl"

	t.Run("html_no_upload_time_fails_closed", func(t *testing.T) {
		t.Parallel()
		// HTML index carries no upload time and no release JSON is available, so
		// a delay filter cannot resolve the publish time and must fail closed.
		up := newFakeUpstream(t, "demo", []upstreamFile{{filename: filename, body: []byte("x")}}, false)
		h := newProxyHarness(t, memblob.OpenBucket(nil),
			pypi.Config{ProxyIndexCacheTTL: -1},
			proxyNamespace("team-proxy", up.server.URL, filter.Spec{Kind: filter.KindDelay, MinAge: "24h"}))

		dl := get(t, h, "/team-proxy/packages/demo/1.0.0/"+filename, "")
		if dl.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (fail closed): %s", dl.StatusCode, readResp(t, dl))
		}
		_ = readResp(t, dl)
	})

	t.Run("release_json_provides_old_upload_time_allows", func(t *testing.T) {
		t.Parallel()
		up := newFakeUpstream(t, "demo", []upstreamFile{{filename: filename, body: []byte("x")}}, false)
		up.mu.Lock()
		up.releaseJSON["1.0.0"] = fmt.Sprintf(`{"urls":[{"filename":%q,"upload_time_iso_8601":"2000-01-01T00:00:00Z"}]}`, filename)
		up.mu.Unlock()
		h := newProxyHarness(t, memblob.OpenBucket(nil),
			pypi.Config{ProxyIndexCacheTTL: -1},
			proxyNamespace("team-proxy", up.server.URL, filter.Spec{Kind: filter.KindDelay, MinAge: "24h"}))

		dl := get(t, h, "/team-proxy/packages/demo/1.0.0/"+filename, "")
		if dl.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 (old enough): %s", dl.StatusCode, readResp(t, dl))
		}
		_ = readResp(t, dl)
	})
}
