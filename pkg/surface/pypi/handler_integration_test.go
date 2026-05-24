package pypi_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"gocloud.dev/blob"
	"gocloud.dev/blob/fileblob"
	"gocloud.dev/blob/memblob"

	"github.com/yolocs/open-artifact/pkg/auth"
	"github.com/yolocs/open-artifact/pkg/core"
	"github.com/yolocs/open-artifact/pkg/metrics"
	"github.com/yolocs/open-artifact/pkg/namespace"
	"github.com/yolocs/open-artifact/pkg/observability"
	"github.com/yolocs/open-artifact/pkg/surface/integrationtest"
	"github.com/yolocs/open-artifact/pkg/surface/pypi"
)

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
	reg    *namespace.Registry
}

func newHarness(t *testing.T, b *blob.Bucket, cfg pypi.Config) *harness {
	t.Helper()
	ctx := t.Context()
	catalog, err := namespace.NewStore(b, "")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	for _, ns := range []*namespace.Namespace{
		integrationtest.HostedAnonymous("team-a"),
		integrationtest.HostedAnonymous("team-b"),
		integrationtest.DenyAll("team-deny"),
		integrationtest.ProxyAnonymous("team-proxy", "https://pypi.org/simple/"),
		integrationtest.ReadOnlyAnonymous("team-readonly"),
	} {
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

type recordingMetrics struct {
	calls []httpMetric
}

type httpMetric struct {
	Format string
	Op     string
	Status string
}

func (r *recordingMetrics) HTTPRequest(format, op, status string, _ time.Duration, _, _ int64) {
	r.calls = append(r.calls, httpMetric{Format: format, Op: op, Status: status})
}
func (r *recordingMetrics) BlobStoreCall(string, string, time.Duration) {}
func (r *recordingMetrics) BlobRedirect(string)                         {}

var _ metrics.Recorder = (*recordingMetrics)(nil)

func upload(t *testing.T, h *harness, namespace, project, version, filename string, body []byte, fields map[string]string) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatalf("WriteField(%s): %v", k, err)
		}
	}
	if err := mw.WriteField("name", project); err != nil {
		t.Fatalf("WriteField(name): %v", err)
	}
	if err := mw.WriteField("version", version); err != nil {
		t.Fatalf("WriteField(version): %v", err)
	}
	part, err := mw.CreateFormFile("content", filename)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := part.Write(body); err != nil {
		t.Fatalf("Write content: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("Close multipart: %v", err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.server.URL+"/"+namespace+"/", &buf)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatalf("Do upload: %v", err)
	}
	return resp
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

func get(t *testing.T, h *harness, path, accept string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, h.server.URL+path, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatalf("Do GET: %v", err)
	}
	return resp
}

func TestHostedUploadIndexesAndDownload(t *testing.T) {
	t.Parallel()

	for _, be := range backends() {
		be := be
		t.Run(be.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, be.open(t), pypi.Config{SimpleIndexCacheTTL: time.Minute})
			body := []byte("wheel bytes")
			resp := upload(t, h, "team-a", "Foo_Bar", "1.0.0", "foo_bar-1.0.0-py3-none-any.whl", body, map[string]string{
				"requires_python": ">=3.11",
				"summary":         "demo package",
			})
			if got := resp.StatusCode; got != http.StatusCreated {
				t.Fatalf("upload status = %d, want %d: %s", got, http.StatusCreated, readResp(t, resp))
			}
			_ = readResp(t, resp)

			dup := upload(t, h, "team-a", "foo-bar", "1.0.0", "foo_bar-1.0.0-py3-none-any.whl", body, nil)
			if got := dup.StatusCode; got != http.StatusConflict {
				t.Fatalf("duplicate status = %d, want %d: %s", got, http.StatusConflict, readResp(t, dup))
			}
			_ = readResp(t, dup)

			root := get(t, h, "/team-a/simple", "")
			if got := root.StatusCode; got != http.StatusOK {
				t.Fatalf("root status = %d, want %d: %s", got, http.StatusOK, readResp(t, root))
			}
			if got := root.Header.Get("Content-Type"); got != "text/html; charset=utf-8" {
				t.Fatalf("root content-type = %q, want text/html; charset=utf-8", got)
			}
			if body := readResp(t, root); !strings.Contains(body, `<a href="foo-bar/">foo-bar</a>`) {
				t.Fatalf("root index missing normalized package: %s", body)
			}

			digest := sha256.Sum256(body)
			wantHash := hex.EncodeToString(digest[:])
			project := get(t, h, "/team-a/simple/foo-bar/", "")
			if got := project.StatusCode; got != http.StatusOK {
				t.Fatalf("project status = %d, want %d: %s", got, http.StatusOK, readResp(t, project))
			}
			projectBody := readResp(t, project)
			for _, want := range []string{
				`/team-a/packages/foo-bar/1.0.0/foo_bar-1.0.0-py3-none-any.whl#sha256=` + wantHash,
				`data-requires-python="&gt;=3.11"`,
			} {
				if !strings.Contains(projectBody, want) {
					t.Fatalf("project index missing %q: %s", want, projectBody)
				}
			}

			jsonResp := get(t, h, "/team-a/simple/Foo_Bar/", "application/vnd.pypi.simple.v1+json")
			if got := jsonResp.StatusCode; got != http.StatusOK {
				t.Fatalf("json status = %d, want %d: %s", got, http.StatusOK, readResp(t, jsonResp))
			}
			if got := jsonResp.Header.Get("Content-Type"); got != "application/vnd.pypi.simple.v1+json" {
				t.Fatalf("json content-type = %q, want PEP 691 content type", got)
			}
			if body := readResp(t, jsonResp); !strings.Contains(body, `"name":"foo-bar"`) || !strings.Contains(body, `"sha256":"`+wantHash+`"`) {
				t.Fatalf("json index missing normalized name or hash: %s", body)
			}

			download := get(t, h, "/team-a/packages/foo-bar/1.0.0/foo_bar-1.0.0-py3-none-any.whl", "")
			if got := download.StatusCode; got != http.StatusOK {
				t.Fatalf("download status = %d, want %d: %s", got, http.StatusOK, readResp(t, download))
			}
			gotBody := []byte(readResp(t, download))
			if diff := cmp.Diff(body, gotBody); diff != "" {
				t.Fatalf("download body mismatch (-want +got):\n%s", diff)
			}

			req, err := http.NewRequestWithContext(t.Context(), http.MethodHead, h.server.URL+"/team-a/packages/foo-bar/1.0.0/foo_bar-1.0.0-py3-none-any.whl", nil)
			if err != nil {
				t.Fatalf("NewRequest HEAD: %v", err)
			}
			head, err := h.server.Client().Do(req)
			if err != nil {
				t.Fatalf("Do HEAD: %v", err)
			}
			defer head.Body.Close()
			if got := head.StatusCode; got != http.StatusOK {
				t.Fatalf("HEAD status = %d, want %d", got, http.StatusOK)
			}
			if b, err := io.ReadAll(head.Body); err != nil || len(b) != 0 {
				t.Fatalf("HEAD body len = %d, err = %v; want empty nil", len(b), err)
			}
		})
	}
}

func TestUploadRejectsMismatchedSHA256Digest(t *testing.T) {
	t.Parallel()

	h := newHarness(t, memblob.OpenBucket(nil), pypi.Config{SimpleIndexCacheTTL: 0})
	resp := upload(t, h, "team-a", "demo", "1.0.0", "demo-1.0.0-py3-none-any.whl", []byte("wheel"), map[string]string{
		"sha256_digest": "000000",
	})
	if got := resp.StatusCode; got != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d: %s", got, http.StatusUnprocessableEntity, readResp(t, resp))
	}
	_ = readResp(t, resp)
}

func TestRootIndexEmptyScopeAndProxyMode(t *testing.T) {
	t.Parallel()

	h := newHarness(t, memblob.OpenBucket(nil), pypi.Config{SimpleIndexCacheTTL: 0})
	empty := get(t, h, "/team-a/simple/", "")
	if empty.StatusCode != http.StatusOK {
		t.Fatalf("empty root status = %d: %s", empty.StatusCode, readResp(t, empty))
	}
	if body := readResp(t, empty); !strings.Contains(body, `pypi:repository-version`) {
		t.Fatalf("empty root missing repository version meta: %s", body)
	}
	proxy := get(t, h, "/team-proxy/simple/", "")
	if proxy.StatusCode != http.StatusNotImplemented {
		t.Fatalf("proxy status = %d, want %d: %s", proxy.StatusCode, http.StatusNotImplemented, readResp(t, proxy))
	}
	_ = readResp(t, proxy)
}

func TestObservabilityLabelsPyPIOperations(t *testing.T) {
	t.Parallel()

	b := memblob.OpenBucket(nil)
	t.Cleanup(func() { b.Close() })
	h := newHarness(t, b, pypi.Config{SimpleIndexCacheTTL: 0})
	rec := &recordingMetrics{}
	observed := observability.Wrap(observability.Config{
		Next:      observability.WrapWithFormat("pypi", h.server.Config.Handler),
		Recorder:  rec,
		Component: "test",
	})
	srv := httptest.NewServer(observed)
	t.Cleanup(srv.Close)

	resp := upload(t, &harness{server: srv, reg: h.reg}, "team-a", "demo", "1.0.0", "demo-1.0.0-py3-none-any.whl", []byte("wheel"), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload status = %d: %s", resp.StatusCode, readResp(t, resp))
	}
	_ = readResp(t, resp)
	root := get(t, &harness{server: srv, reg: h.reg}, "/team-a/simple/", "")
	if root.StatusCode != http.StatusOK {
		t.Fatalf("root status = %d: %s", root.StatusCode, readResp(t, root))
	}
	_ = readResp(t, root)

	want := []httpMetric{
		{Format: "pypi", Op: "upload", Status: "201"},
		{Format: "pypi", Op: "simple.root", Status: "200"},
	}
	if diff := cmp.Diff(want, rec.calls); diff != "" {
		t.Fatalf("metrics mismatch (-want +got):\n%s", diff)
	}
}

func TestProjectIndexCacheInvalidatesOnUpload(t *testing.T) {
	t.Parallel()

	h := newHarness(t, memblob.OpenBucket(nil), pypi.Config{SimpleIndexCacheTTL: time.Minute})
	first := upload(t, h, "team-a", "demo", "1.0.0", "demo-1.0.0-py3-none-any.whl", []byte("first"), nil)
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("first upload status = %d: %s", first.StatusCode, readResp(t, first))
	}
	_ = readResp(t, first)
	before := readResp(t, get(t, h, "/team-a/simple/demo/", ""))
	if !strings.Contains(before, "demo-1.0.0-py3-none-any.whl") {
		t.Fatalf("index missing first file: %s", before)
	}

	scoped, err := h.reg.For("team-a", core.FormatPyPI.String())
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	store, err := scoped.Store()
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if _, err := store.Package("demo").Version("2.0.0").AddFile(t.Context(), "demo-2.0.0-py3-none-any.whl", strings.NewReader("direct")); err != nil {
		t.Fatalf("direct AddFile: %v", err)
	}
	cached := readResp(t, get(t, h, "/team-a/simple/demo/", ""))
	if strings.Contains(cached, "demo-2.0.0-py3-none-any.whl") {
		t.Fatalf("direct store write unexpectedly bypassed cache: %s", cached)
	}

	second := upload(t, h, "team-a", "demo", "3.0.0", "demo-3.0.0-py3-none-any.whl", []byte("second"), nil)
	if second.StatusCode != http.StatusCreated {
		t.Fatalf("second upload status = %d: %s", second.StatusCode, readResp(t, second))
	}
	_ = readResp(t, second)
	after := readResp(t, get(t, h, "/team-a/simple/demo/", ""))
	for _, want := range []string{"demo-2.0.0-py3-none-any.whl", "demo-3.0.0-py3-none-any.whl"} {
		if !strings.Contains(after, want) {
			t.Fatalf("index after invalidation missing %q: %s", want, after)
		}
	}
}

func TestNamespaceAuthorizationAndIsolation(t *testing.T) {
	t.Parallel()

	h := newHarness(t, memblob.OpenBucket(nil), pypi.Config{})
	body := []byte("wheel")
	cases := []struct {
		name      string
		namespace string
		want      int
	}{
		{name: "unknown namespace", namespace: "missing", want: http.StatusNotFound},
		{name: "deny all namespace", namespace: "team-deny", want: http.StatusForbidden},
		{name: "read only upload denied", namespace: "team-readonly", want: http.StatusForbidden},
	}

	t.Run("denied uploads", func(t *testing.T) {
		for _, tc := range cases {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()

				resp := upload(t, h, tc.namespace, "demo", "1.0.0", "demo-1.0.0-py3-none-any.whl", body, nil)
				if got := resp.StatusCode; got != tc.want {
					t.Fatalf("status = %d, want %d: %s", got, tc.want, readResp(t, resp))
				}
				_ = readResp(t, resp)
			})
		}
	})

	ok := upload(t, h, "team-a", "demo", "1.0.0", "demo-1.0.0-py3-none-any.whl", body, nil)
	if ok.StatusCode != http.StatusCreated {
		t.Fatalf("team-a upload = %d: %s", ok.StatusCode, readResp(t, ok))
	}
	_ = readResp(t, ok)
	if got := readResp(t, get(t, h, "/team-b/simple/demo/", "")); !strings.Contains(got, "not found") {
		t.Fatalf("team-b saw team-a package or unexpected body: %s", got)
	}
}

func TestConcurrentUploadsDifferentFilesSameVersion(t *testing.T) {
	t.Parallel()

	h := newHarness(t, memblob.OpenBucket(nil), pypi.Config{SimpleIndexCacheTTL: 0})
	files := []string{
		"demo-1.0.0-py3-none-any.whl",
		"demo-1.0.0.tar.gz",
	}
	var wg sync.WaitGroup
	for _, file := range files {
		file := file
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := upload(t, h, "team-a", "demo", "1.0.0", file, []byte(file), nil)
			if resp.StatusCode != http.StatusCreated {
				t.Errorf("%s upload status = %d: %s", file, resp.StatusCode, readResp(t, resp))
				return
			}
			_ = readResp(t, resp)
		}()
	}
	wg.Wait()

	body := readResp(t, get(t, h, "/team-a/simple/demo/", ""))
	for _, file := range files {
		if !strings.Contains(body, file) {
			t.Fatalf("index missing %s after concurrent uploads: %s", file, body)
		}
	}
}
