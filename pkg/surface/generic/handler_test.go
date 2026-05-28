package generic_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gocloud.dev/blob"
	"gocloud.dev/blob/fileblob"
	"gocloud.dev/blob/memblob"

	"github.com/yolocs/open-artifact/pkg/auth"
	"github.com/yolocs/open-artifact/pkg/namespace"
	"github.com/yolocs/open-artifact/pkg/surface/generic"
	"github.com/yolocs/open-artifact/pkg/surface/integrationtest"
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
	client *http.Client
}

func newHarness(t *testing.T, b *blob.Bucket, cfg generic.Config) *harness {
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
		integrationtest.ReadOnlyAnonymous("team-readonly"),
		integrationtest.ProxyAnonymous("team-proxy", "https://example.test/"),
	} {
		if err := integrationtest.SeedNamespace(ctx, catalog, ns); err != nil {
			t.Fatalf("SeedNamespace(%s): %v", ns.Name, err)
		}
	}
	reg, err := namespace.NewRegistry(b, "", catalog)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	srv := httptest.NewServer(generic.Handler(reg, auth.AlwaysAnonymous{}, cfg))
	t.Cleanup(srv.Close)
	return &harness{server: srv, client: srv.Client()}
}

func (h *harness) do(t *testing.T, method, path string, body io.Reader, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, h.server.URL+path, body)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("Do %s %s: %v", method, path, err)
	}
	return resp
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

func decode[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	defer resp.Body.Close()
	var v T
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return v
}

func eachBackend(t *testing.T, fn func(t *testing.T, b backend)) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			fn(t, b)
		})
	}
}

type pkgListResp struct {
	Namespace string   `json:"namespace"`
	Packages  []string `json:"packages"`
}

type pkgResp struct {
	Name        string         `json:"name"`
	Annotations map[string]any `json:"annotations"`
	Versions    []string       `json:"versions"`
}

type verResp struct {
	Package     string         `json:"package"`
	Version     string         `json:"version"`
	Annotations map[string]any `json:"annotations"`
	Files       []string       `json:"files"`
}

type fileItem struct {
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	Digest      string `json:"digest"`
	ContentType string `json:"content_type"`
}

type fileListResp struct {
	Files []fileItem `json:"files"`
}

type uploadResp struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
}

const fileBase = "/team-a/generic/packages/app/versions/1.0.0/files/"

func TestPackageLifecycle(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, be backend) {
		h := newHarness(t, be.open(t), generic.Config{})

		if resp := h.do(t, http.MethodPut, "/team-a/generic/packages/app", nil, nil); resp.StatusCode != http.StatusCreated {
			t.Fatalf("create package = %d, want 201 (%s)", resp.StatusCode, body(t, resp))
		}
		// Re-PUT is an idempotent update.
		if resp := h.do(t, http.MethodPut, "/team-a/generic/packages/app", nil, nil); resp.StatusCode != http.StatusOK {
			t.Fatalf("re-create package = %d, want 200 (%s)", resp.StatusCode, body(t, resp))
		}

		// PUT with annotations updates and is reflected on read.
		annBody := strings.NewReader(`{"annotations":{"team":"a","tier":"gold"}}`)
		if resp := h.do(t, http.MethodPut, "/team-a/generic/packages/app", annBody, nil); resp.StatusCode != http.StatusOK {
			t.Fatalf("annotate package = %d, want 200", resp.StatusCode)
		}

		got := decode[pkgResp](t, h.do(t, http.MethodGet, "/team-a/generic/packages/app", nil, nil))
		if got.Name != "app" {
			t.Errorf("package name = %q, want app", got.Name)
		}
		if got.Annotations["team"] != "a" || got.Annotations["tier"] != "gold" {
			t.Errorf("annotations = %v, want team=a tier=gold", got.Annotations)
		}
		if len(got.Versions) != 0 {
			t.Errorf("versions = %v, want empty", got.Versions)
		}

		list := decode[pkgListResp](t, h.do(t, http.MethodGet, "/team-a/generic/packages", nil, nil))
		if len(list.Packages) != 1 || list.Packages[0] != "app" {
			t.Errorf("packages = %v, want [app]", list.Packages)
		}
	})
}

func TestVersionLifecycle(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, be backend) {
		h := newHarness(t, be.open(t), generic.Config{})

		// PUT version auto-creates the package.
		if resp := h.do(t, http.MethodPut, "/team-a/generic/packages/app/versions/1.0.0", nil, nil); resp.StatusCode != http.StatusCreated {
			t.Fatalf("create version = %d, want 201 (%s)", resp.StatusCode, body(t, resp))
		}

		pkg := decode[pkgResp](t, h.do(t, http.MethodGet, "/team-a/generic/packages/app", nil, nil))
		if len(pkg.Versions) != 1 || pkg.Versions[0] != "1.0.0" {
			t.Errorf("package versions = %v, want [1.0.0]", pkg.Versions)
		}

		ver := decode[verResp](t, h.do(t, http.MethodGet, "/team-a/generic/packages/app/versions/1.0.0", nil, nil))
		if ver.Version != "1.0.0" || len(ver.Files) != 0 {
			t.Errorf("version = %+v, want version 1.0.0 with no files", ver)
		}

		// Listing versions of a missing package is an empty list, not 404.
		missing := h.do(t, http.MethodGet, "/team-a/generic/packages/nope/versions", nil, nil)
		if missing.StatusCode != http.StatusOK {
			t.Errorf("list versions of missing package = %d, want 200", missing.StatusCode)
		}
	})
}

func TestFileUploadDownload(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, be backend) {
		h := newHarness(t, be.open(t), generic.Config{})

		content := "hello generic world"
		sum := sha256.Sum256([]byte(content))
		wantDigest := "sha256:" + hex.EncodeToString(sum[:])

		up := h.do(t, http.MethodPut, fileBase+"app-1.0.0.bin", strings.NewReader(content), map[string]string{
			"Content-Type": "application/x-thing",
		})
		if up.StatusCode != http.StatusCreated {
			t.Fatalf("upload = %d, want 201 (%s)", up.StatusCode, body(t, up))
		}
		upBody := decode[uploadResp](t, up)
		if upBody.Digest != wantDigest {
			t.Errorf("upload digest = %q, want %q", upBody.Digest, wantDigest)
		}
		if upBody.Size != int64(len(content)) {
			t.Errorf("upload size = %d, want %d", upBody.Size, len(content))
		}

		// Download returns the bytes and echoes the stored Content-Type.
		dl := h.do(t, http.MethodGet, fileBase+"app-1.0.0.bin", nil, nil)
		if dl.StatusCode != http.StatusOK {
			t.Fatalf("download = %d, want 200", dl.StatusCode)
		}
		if ct := dl.Header.Get("Content-Type"); ct != "application/x-thing" {
			t.Errorf("download Content-Type = %q, want application/x-thing", ct)
		}
		if got := body(t, dl); got != content {
			t.Errorf("download body = %q, want %q", got, content)
		}

		// HEAD reports size and digest without a body.
		head := h.do(t, http.MethodHead, fileBase+"app-1.0.0.bin", nil, nil)
		if head.StatusCode != http.StatusOK {
			t.Fatalf("head = %d, want 200", head.StatusCode)
		}
		if cl := head.Header.Get("Content-Length"); cl != "19" {
			t.Errorf("head Content-Length = %q, want 19", cl)
		}
		if got := body(t, head); got != "" {
			t.Errorf("head body = %q, want empty", got)
		}

		// File listing carries metadata.
		fl := decode[fileListResp](t, h.do(t, http.MethodGet, "/team-a/generic/packages/app/versions/1.0.0/files", nil, nil))
		if len(fl.Files) != 1 {
			t.Fatalf("files = %v, want 1 file", fl.Files)
		}
		f := fl.Files[0]
		if f.Name != "app-1.0.0.bin" || f.Digest != wantDigest || f.ContentType != "application/x-thing" {
			t.Errorf("file item = %+v", f)
		}

		// Re-upload of the same file is rejected (immutable).
		if resp := h.do(t, http.MethodPut, fileBase+"app-1.0.0.bin", strings.NewReader("other"), nil); resp.StatusCode != http.StatusConflict {
			t.Errorf("re-upload = %d, want 409", resp.StatusCode)
		}

		// Missing file is 404.
		if resp := h.do(t, http.MethodGet, fileBase+"missing.bin", nil, nil); resp.StatusCode != http.StatusNotFound {
			t.Errorf("download missing = %d, want 404", resp.StatusCode)
		}
	})
}

func TestOverwriteAllowed(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, be backend) {
		h := newHarness(t, be.open(t), generic.Config{AllowOverwrite: true})

		if resp := h.do(t, http.MethodPut, fileBase+"f.bin", strings.NewReader("v1"), nil); resp.StatusCode != http.StatusCreated {
			t.Fatalf("first upload = %d, want 201", resp.StatusCode)
		}
		if resp := h.do(t, http.MethodPut, fileBase+"f.bin", strings.NewReader("v2-longer"), nil); resp.StatusCode != http.StatusCreated {
			t.Fatalf("overwrite upload = %d, want 201", resp.StatusCode)
		}
		if got := body(t, h.do(t, http.MethodGet, fileBase+"f.bin", nil, nil)); got != "v2-longer" {
			t.Errorf("after overwrite = %q, want v2-longer", got)
		}
	})
}

func TestChecksumVerification(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, be backend) {
		h := newHarness(t, be.open(t), generic.Config{})

		content := "checksum me"
		sum := sha256.Sum256([]byte(content))
		good := hex.EncodeToString(sum[:])

		if resp := h.do(t, http.MethodPut, fileBase+"ok.bin", strings.NewReader(content), map[string]string{
			"X-Checksum-Sha256": good,
		}); resp.StatusCode != http.StatusCreated {
			t.Fatalf("matching checksum upload = %d, want 201 (%s)", resp.StatusCode, body(t, resp))
		}

		if resp := h.do(t, http.MethodPut, fileBase+"bad.bin", strings.NewReader(content), map[string]string{
			"X-Checksum-Sha256": strings.Repeat("00", 32),
		}); resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("mismatched checksum upload = %d, want 422", resp.StatusCode)
		}

		if resp := h.do(t, http.MethodPut, fileBase+"malformed.bin", strings.NewReader(content), map[string]string{
			"X-Checksum-Sha256": "not-hex",
		}); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("malformed checksum header = %d, want 400", resp.StatusCode)
		}
	})
}

func TestUploadTooLarge(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, be backend) {
		h := newHarness(t, be.open(t), generic.Config{MaxUploadBytes: 8})
		resp := h.do(t, http.MethodPut, fileBase+"big.bin", strings.NewReader("123456789"), nil)
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Errorf("oversized upload = %d, want 413 (%s)", resp.StatusCode, body(t, resp))
		}
	})
}

func TestAuthorization(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, be backend) {
		h := newHarness(t, be.open(t), generic.Config{})

		// Deny-all namespace rejects reads and writes.
		if resp := h.do(t, http.MethodGet, "/team-deny/generic/packages", nil, nil); resp.StatusCode != http.StatusForbidden {
			t.Errorf("deny read = %d, want 403", resp.StatusCode)
		}
		if resp := h.do(t, http.MethodPut, "/team-deny/generic/packages/p", nil, nil); resp.StatusCode != http.StatusForbidden {
			t.Errorf("deny write = %d, want 403", resp.StatusCode)
		}

		// Read-only namespace allows reads, rejects writes.
		if resp := h.do(t, http.MethodGet, "/team-readonly/generic/packages", nil, nil); resp.StatusCode != http.StatusOK {
			t.Errorf("readonly read = %d, want 200", resp.StatusCode)
		}
		if resp := h.do(t, http.MethodPut, "/team-readonly/generic/packages/p/versions/1/files/f.bin", strings.NewReader("x"), nil); resp.StatusCode != http.StatusForbidden {
			t.Errorf("readonly write = %d, want 403", resp.StatusCode)
		}

		// Unknown namespace is 404.
		if resp := h.do(t, http.MethodGet, "/team-ghost/generic/packages", nil, nil); resp.StatusCode != http.StatusNotFound {
			t.Errorf("unknown namespace = %d, want 404", resp.StatusCode)
		}
	})
}

func TestProxyModeUnsupported(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, be backend) {
		h := newHarness(t, be.open(t), generic.Config{})
		cases := []struct {
			method, path string
		}{
			{http.MethodGet, "/team-proxy/generic/packages"},
			{http.MethodPut, "/team-proxy/generic/packages/p"},
			{http.MethodPut, "/team-proxy/generic/packages/p/versions/1/files/f.bin"},
		}
		for _, c := range cases {
			resp := h.do(t, c.method, c.path, nil, nil)
			if resp.StatusCode != http.StatusNotImplemented {
				t.Errorf("%s %s proxy namespace = %d, want 501", c.method, c.path, resp.StatusCode)
			}
		}
	})
}

func TestDeleteDeferred(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, be backend) {
		h := newHarness(t, be.open(t), generic.Config{})
		resp := h.do(t, http.MethodDelete, fileBase+"f.bin", nil, nil)
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("delete = %d, want 405", resp.StatusCode)
		}
		if allow := resp.Header.Get("Allow"); !strings.Contains(allow, "PUT") || !strings.Contains(allow, "GET") {
			t.Errorf("Allow = %q, want it to list GET and PUT", allow)
		}
	})
}

func TestInvalidNames(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, be backend) {
		h := newHarness(t, be.open(t), generic.Config{})
		// Leading-dot names are rejected by the Store.
		if resp := h.do(t, http.MethodPut, "/team-a/generic/packages/.secret", nil, nil); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("leading-dot package = %d, want 400", resp.StatusCode)
		}
		if resp := h.do(t, http.MethodPut, "/team-a/generic/packages/app/versions/1.0.0/files/.hidden", strings.NewReader("x"), nil); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("leading-dot file = %d, want 400", resp.StatusCode)
		}
	})
}

func TestNamespaceIsolation(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, be backend) {
		h := newHarness(t, be.open(t), generic.Config{})
		if resp := h.do(t, http.MethodPut, "/team-a/generic/packages/shared/versions/1/files/f.bin", strings.NewReader("a"), nil); resp.StatusCode != http.StatusCreated {
			t.Fatalf("upload to team-a = %d, want 201", resp.StatusCode)
		}
		if resp := h.do(t, http.MethodGet, "/team-b/generic/packages/shared", nil, nil); resp.StatusCode != http.StatusNotFound {
			t.Errorf("team-b sees team-a package = %d, want 404", resp.StatusCode)
		}
		list := decode[pkgListResp](t, h.do(t, http.MethodGet, "/team-b/generic/packages", nil, nil))
		if len(list.Packages) != 0 {
			t.Errorf("team-b packages = %v, want empty", list.Packages)
		}
	})
}
