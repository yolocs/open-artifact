package npm_test

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"gocloud.dev/blob"
	"gocloud.dev/blob/fileblob"
	"gocloud.dev/blob/memblob"

	"github.com/yolocs/open-artifact/pkg/auth"
	"github.com/yolocs/open-artifact/pkg/namespace"
	"github.com/yolocs/open-artifact/pkg/surface/integrationtest"
	"github.com/yolocs/open-artifact/pkg/surface/npm"
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

func newHarness(t *testing.T, b *blob.Bucket, cfg npm.Config) *harness {
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
		integrationtest.ProxyAnonymous("team-proxy", "https://registry.npmjs.org/"),
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
	srv := httptest.NewServer(npm.Handler(reg, auth.AlwaysAnonymous{}, cfg))
	t.Cleanup(srv.Close)
	return &harness{server: srv, reg: reg}
}

// tarballName builds the npm attachment filename for a package version.
func tarballName(npmName, version string) string {
	unscoped := npmName
	if i := strings.IndexByte(npmName, '/'); i >= 0 {
		unscoped = npmName[i+1:]
	}
	return unscoped + "-" + version + ".tgz"
}

func sha1Hex(b []byte) string {
	sum := sha1.Sum(b)
	return hex.EncodeToString(sum[:])
}

func sha512SRI(b []byte) string {
	sum := sha512.Sum512(b)
	return "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
}

// publishDoc builds a CouchDB-shaped npm publish document for a single version.
func publishDoc(npmName, version string, tarball []byte, distTags map[string]string) map[string]any {
	filename := tarballName(npmName, version)
	meta := map[string]any{
		"name":    npmName,
		"version": version,
		"dist": map[string]any{
			"shasum":    sha1Hex(tarball),
			"integrity": sha512SRI(tarball),
			// A publisher-provided URL that must be rewritten away on store/read.
			"tarball": "https://registry.npmjs.org/" + npmName + "/-/" + filename,
		},
	}
	doc := map[string]any{
		"_id":      npmName,
		"name":     npmName,
		"versions": map[string]any{version: meta},
		"_attachments": map[string]any{
			filename: map[string]any{
				"content_type": "application/octet-stream",
				"data":         base64.StdEncoding.EncodeToString(tarball),
				"length":       len(tarball),
			},
		},
	}
	if distTags != nil {
		doc["dist-tags"] = distTags
	}
	return doc
}

// urlName renders an npm package name for use in a request path. Scoped names
// use the %2f-encoded form the npm CLI sends.
func urlName(npmName string) string {
	return strings.Replace(npmName, "/", "%2f", 1)
}

func put(t *testing.T, h *harness, ns, path string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, h.server.URL+"/"+ns+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatalf("Do PUT: %v", err)
	}
	return resp
}

func publish(t *testing.T, h *harness, ns, npmName string, doc map[string]any) *http.Response {
	t.Helper()
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("Marshal doc: %v", err)
	}
	return put(t, h, ns, "/"+urlName(npmName), body)
}

func do(t *testing.T, h *harness, method, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, h.server.URL+path, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatalf("Do %s: %v", method, err)
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

func decodeJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode json: %v", err)
	}
	return out
}

func TestPublishPackumentAndDownload(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		pkg     string
		urlPath string // packument path under the namespace
	}{
		{name: "unscoped", pkg: "left-pad", urlPath: "/left-pad"},
		{name: "scoped", pkg: "@scope/pkg", urlPath: "/@scope%2fpkg"},
	}

	for _, be := range backends() {
		be := be
		t.Run(be.name, func(t *testing.T) {
			t.Parallel()
			for _, tc := range cases {
				tc := tc
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					h := newHarness(t, be.open(t), npm.Config{})
					tarball := []byte("tarball bytes for " + tc.pkg)

					resp := publish(t, h, "team-a", tc.pkg, publishDoc(tc.pkg, "1.0.0", tarball, nil))
					if got := resp.StatusCode; got != http.StatusCreated {
						t.Fatalf("publish status = %d, want 201: %s", got, readResp(t, resp))
					}
					_ = readResp(t, resp)

					// Packument assembly.
					pkmt := decodeJSON(t, do(t, h, http.MethodGet, "/team-a"+tc.urlPath))
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
					gotTarball, _ := dist["tarball"].(string)
					wantTarball := h.server.URL + "/team-a" + tarballPath(tc.pkg, "1.0.0")
					if gotTarball != wantTarball {
						t.Fatalf("tarball URL = %q, want %q (must point back at open-artifact)", gotTarball, wantTarball)
					}

					// Download the tarball that the packument advertises.
					dl := do(t, h, http.MethodGet, "/team-a"+tarballPath(tc.pkg, "1.0.0"))
					if dl.StatusCode != http.StatusOK {
						t.Fatalf("download status = %d: %s", dl.StatusCode, readResp(t, dl))
					}
					if got := dl.Header.Get("Content-Length"); got != strconv.Itoa(len(tarball)) {
						t.Fatalf("Content-Length = %q, want %d", got, len(tarball))
					}
					if dl.Header.Get("ETag") == "" {
						t.Fatalf("download missing ETag")
					}
					if diff := cmp.Diff(tarball, []byte(readResp(t, dl))); diff != "" {
						t.Fatalf("tarball body mismatch (-want +got):\n%s", diff)
					}

					// HEAD returns no body.
					head := do(t, h, http.MethodHead, "/team-a"+tarballPath(tc.pkg, "1.0.0"))
					if head.StatusCode != http.StatusOK {
						t.Fatalf("HEAD status = %d", head.StatusCode)
					}
					if b := readResp(t, head); b != "" {
						t.Fatalf("HEAD body = %q, want empty", b)
					}
				})
			}
		})
	}
}

// tarballPath renders the registry-rooted tarball path (without namespace) for
// a package version.
func tarballPath(npmName, version string) string {
	return "/" + npmName + "/-/" + tarballName(npmName, version)
}

func TestPackumentAssemblesManyVersionsConcurrently(t *testing.T) {
	t.Parallel()

	h := newHarness(t, memblob.OpenBucket(nil), npm.Config{})
	versions := []string{"1.0.0", "1.1.0", "1.2.0", "2.0.0", "2.1.0", "3.0.0", "3.0.1", "3.1.0"}
	for _, v := range versions {
		resp := publish(t, h, "team-a", "left-pad", publishDoc("left-pad", v, []byte("body "+v), nil))
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("publish %s = %d: %s", v, resp.StatusCode, readResp(t, resp))
		}
		_ = readResp(t, resp)
	}

	pkmt := decodeJSON(t, do(t, h, http.MethodGet, "/team-a/left-pad"))
	got, _ := pkmt["versions"].(map[string]any)
	if len(got) != len(versions) {
		t.Fatalf("packument has %d versions, want %d", len(got), len(versions))
	}
	for _, v := range versions {
		entry, ok := got[v].(map[string]any)
		if !ok {
			t.Fatalf("packument missing version %s", v)
		}
		dist, _ := entry["dist"].(map[string]any)
		want := h.server.URL + "/team-a" + tarballPath("left-pad", v)
		if dist["tarball"] != want {
			t.Fatalf("version %s tarball = %v, want %q", v, dist["tarball"], want)
		}
	}
}

func TestDuplicatePublishConflicts(t *testing.T) {
	t.Parallel()

	h := newHarness(t, memblob.OpenBucket(nil), npm.Config{})
	doc := publishDoc("left-pad", "1.0.0", []byte("v1"), nil)
	if resp := publish(t, h, "team-a", "left-pad", doc); resp.StatusCode != http.StatusCreated {
		t.Fatalf("first publish = %d: %s", resp.StatusCode, readResp(t, resp))
	}
	dup := publish(t, h, "team-a", "left-pad", doc)
	if dup.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate publish = %d, want 409: %s", dup.StatusCode, readResp(t, dup))
	}
	_ = readResp(t, dup)
}

func TestPublishRejectsIntegrityMismatch(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		mutate func(dist map[string]any)
	}{
		{name: "shasum", mutate: func(dist map[string]any) { dist["shasum"] = strings.Repeat("0", 40) }},
		{name: "integrity", mutate: func(dist map[string]any) {
			dist["integrity"] = "sha512-" + base64.StdEncoding.EncodeToString(make([]byte, 64))
		}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, memblob.OpenBucket(nil), npm.Config{})
			doc := publishDoc("left-pad", "1.0.0", []byte("payload"), nil)
			versions := doc["versions"].(map[string]any)
			dist := versions["1.0.0"].(map[string]any)["dist"].(map[string]any)
			tc.mutate(dist)
			resp := publish(t, h, "team-a", "left-pad", doc)
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422: %s", resp.StatusCode, readResp(t, resp))
			}
			_ = readResp(t, resp)
		})
	}
}

func TestPublishRejectsMultipleVersions(t *testing.T) {
	t.Parallel()

	h := newHarness(t, memblob.OpenBucket(nil), npm.Config{})
	doc := publishDoc("left-pad", "1.0.0", []byte("v1"), nil)
	versions := doc["versions"].(map[string]any)
	versions["2.0.0"] = map[string]any{"name": "left-pad", "version": "2.0.0", "dist": map[string]any{}}
	resp := publish(t, h, "team-a", "left-pad", doc)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, readResp(t, resp))
	}
	_ = readResp(t, resp)
}

func TestPublishNameMismatch(t *testing.T) {
	t.Parallel()

	h := newHarness(t, memblob.OpenBucket(nil), npm.Config{})
	doc := publishDoc("left-pad", "1.0.0", []byte("v1"), nil)
	// PUT to a different URL package than the body name.
	body, _ := json.Marshal(doc)
	resp := put(t, h, "team-a", "/right-pad", body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, readResp(t, resp))
	}
	_ = readResp(t, resp)
}

func TestDistTags(t *testing.T) {
	t.Parallel()

	h := newHarness(t, memblob.OpenBucket(nil), npm.Config{})
	if resp := publish(t, h, "team-a", "left-pad", publishDoc("left-pad", "1.0.0", []byte("v1"), nil)); resp.StatusCode != http.StatusCreated {
		t.Fatalf("publish 1.0.0 = %d: %s", resp.StatusCode, readResp(t, resp))
	} else {
		_ = readResp(t, resp)
	}
	if resp := publish(t, h, "team-a", "left-pad", publishDoc("left-pad", "2.0.0", []byte("v2"), nil)); resp.StatusCode != http.StatusCreated {
		t.Fatalf("publish 2.0.0 = %d: %s", resp.StatusCode, readResp(t, resp))
	} else {
		_ = readResp(t, resp)
	}

	// Add a beta tag pointing at 2.0.0.
	add := put(t, h, "team-a", "/-/package/left-pad/dist-tags/beta", []byte(`"2.0.0"`))
	if add.StatusCode != http.StatusOK {
		t.Fatalf("dist-tag add = %d, want 200: %s", add.StatusCode, readResp(t, add))
	}
	_ = readResp(t, add)

	// List dist-tags.
	list := decodeJSON(t, do(t, h, http.MethodGet, "/team-a/-/package/left-pad/dist-tags"))
	want := map[string]any{"latest": "2.0.0", "beta": "2.0.0"}
	if diff := cmp.Diff(want, list); diff != "" {
		t.Fatalf("dist-tags mismatch (-want +got):\n%s", diff)
	}

	// Adding a tag for a missing version is a 404.
	missing := put(t, h, "team-a", "/-/package/left-pad/dist-tags/next", []byte(`"9.9.9"`))
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("dist-tag add missing version = %d, want 404: %s", missing.StatusCode, readResp(t, missing))
	}
	_ = readResp(t, missing)

	// DELETE is not implemented in v1.
	del := do(t, h, http.MethodDelete, "/team-a/-/package/left-pad/dist-tags/beta")
	if del.StatusCode != http.StatusNotImplemented {
		t.Fatalf("dist-tag delete = %d, want 501: %s", del.StatusCode, readResp(t, del))
	}
	_ = readResp(t, del)
}

func TestPackumentNotFound(t *testing.T) {
	t.Parallel()

	h := newHarness(t, memblob.OpenBucket(nil), npm.Config{})
	resp := do(t, h, http.MethodGet, "/team-a/does-not-exist")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", resp.StatusCode, readResp(t, resp))
	}
	_ = readResp(t, resp)
}

func TestPingAndRoot(t *testing.T) {
	t.Parallel()

	h := newHarness(t, memblob.OpenBucket(nil), npm.Config{})
	ping := do(t, h, http.MethodGet, "/team-a/-/ping")
	if ping.StatusCode != http.StatusOK {
		t.Fatalf("ping status = %d: %s", ping.StatusCode, readResp(t, ping))
	}
	if body := strings.TrimSpace(readResp(t, ping)); body != "{}" {
		t.Fatalf("ping body = %q, want {}", body)
	}
	root := do(t, h, http.MethodGet, "/team-a")
	if root.StatusCode != http.StatusOK {
		t.Fatalf("root status = %d: %s", root.StatusCode, readResp(t, root))
	}
}

func TestProxyModeRejected(t *testing.T) {
	t.Parallel()

	h := newHarness(t, memblob.OpenBucket(nil), npm.Config{})
	// Writes in proxy mode are rejected (405); reads are not implemented (501, #22).
	pub := publish(t, h, "team-proxy", "left-pad", publishDoc("left-pad", "1.0.0", []byte("v1"), nil))
	if pub.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("proxy publish = %d, want 405: %s", pub.StatusCode, readResp(t, pub))
	}
	_ = readResp(t, pub)
	read := do(t, h, http.MethodGet, "/team-proxy/left-pad")
	if read.StatusCode != http.StatusNotImplemented {
		t.Fatalf("proxy read = %d, want 501: %s", read.StatusCode, readResp(t, read))
	}
	_ = readResp(t, read)
}

func TestAuthorizationAndIsolation(t *testing.T) {
	t.Parallel()

	h := newHarness(t, memblob.OpenBucket(nil), npm.Config{})
	cases := []struct {
		name string
		ns   string
		want int
	}{
		{name: "unknown namespace", ns: "missing", want: http.StatusNotFound},
		{name: "deny all", ns: "team-deny", want: http.StatusForbidden},
		{name: "read only publish denied", ns: "team-readonly", want: http.StatusForbidden},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resp := publish(t, h, tc.ns, "left-pad", publishDoc("left-pad", "1.0.0", []byte("v1"), nil))
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, tc.want, readResp(t, resp))
			}
			_ = readResp(t, resp)
		})
	}

	// team-a publish is invisible to team-b.
	if resp := publish(t, h, "team-a", "left-pad", publishDoc("left-pad", "1.0.0", []byte("v1"), nil)); resp.StatusCode != http.StatusCreated {
		t.Fatalf("team-a publish = %d: %s", resp.StatusCode, readResp(t, resp))
	} else {
		_ = readResp(t, resp)
	}
	other := do(t, h, http.MethodGet, "/team-b/left-pad")
	if other.StatusCode != http.StatusNotFound {
		t.Fatalf("team-b cross-read = %d, want 404: %s", other.StatusCode, readResp(t, other))
	}
	_ = readResp(t, other)
}

func TestInvalidPackageName(t *testing.T) {
	t.Parallel()

	h := newHarness(t, memblob.OpenBucket(nil), npm.Config{})
	// Uppercase is rejected by npm name rules.
	resp := do(t, h, http.MethodGet, "/team-a/Left-Pad")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, readResp(t, resp))
	}
	_ = readResp(t, resp)
}
