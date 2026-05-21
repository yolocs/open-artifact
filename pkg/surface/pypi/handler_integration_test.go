//go:build integration

package pypi

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"gocloud.dev/blob/memblob"

	"github.com/yolocs/open-artifact/pkg/core"
	"github.com/yolocs/open-artifact/pkg/core/blobstore"
)

const testScope = "pypi/global"

// newStore returns a real blobstore.Store over a fresh memblob bucket.
func newStore(t *testing.T) core.Store {
	t.Helper()
	b := memblob.OpenBucket(nil)
	t.Cleanup(func() { b.Close() })
	s, err := blobstore.NewWithBucket(b, testScope)
	if err != nil {
		t.Fatalf("NewWithBucket: %v", err)
	}
	return s
}

// serve mounts h at the root of a fresh httptest server and returns it plus a
// client that does not follow redirects (so 307s are observable).
func serve(t *testing.T, h *Handler) (*httptest.Server, *http.Client) {
	t.Helper()
	mux := http.NewServeMux()
	h.Mount("", mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return srv, client
}

// upload posts a twine-style multipart upload and returns the response.
func upload(t *testing.T, srv *httptest.Server, client *http.Client, name, version, filename string, content []byte) *http.Response {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField(":action", "file_upload")
	_ = mw.WriteField("name", name)
	_ = mw.WriteField("version", version)
	fw, err := mw.CreateFormFile("content", filename)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatalf("write content: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/", &body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("upload do: %v", err)
	}
	return resp
}

func get(t *testing.T, client *http.Client, url string, accept string) *http.Response {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	return resp
}

func TestUploadDownloadRoundTrip(t *testing.T) {
	t.Parallel()
	h := NewHandler(newStore(t), nil)
	srv, client := serve(t, h)

	content := []byte("wheel-bytes-here")
	resp := upload(t, srv, client, "Requests", "2.31.0", "requests-2.31.0-py3-none-any.whl", content)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upload status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// Download the file (normalized package name).
	dl := get(t, client, srv.URL+"/packages/requests/2.31.0/requests-2.31.0-py3-none-any.whl", "")
	defer dl.Body.Close()
	if dl.StatusCode != http.StatusOK {
		t.Fatalf("download status = %d, want 200", dl.StatusCode)
	}
	got, _ := io.ReadAll(dl.Body)
	if !bytes.Equal(got, content) {
		t.Errorf("download body = %q, want %q", got, content)
	}

	// HEAD short-circuits to a presence check.
	headReq, _ := http.NewRequestWithContext(t.Context(), http.MethodHead, srv.URL+"/packages/requests/2.31.0/requests-2.31.0-py3-none-any.whl", nil)
	head, err := client.Do(headReq)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	head.Body.Close()
	if head.StatusCode != http.StatusOK {
		t.Errorf("HEAD status = %d, want 200", head.StatusCode)
	}
}

func TestProjectIndexHTMLAndJSON(t *testing.T) {
	t.Parallel()
	h := NewHandler(newStore(t), nil)
	srv, client := serve(t, h)

	upload(t, srv, client, "requests", "2.31.0", "requests-2.31.0-py3-none-any.whl", []byte("whl")).Body.Close()
	upload(t, srv, client, "requests", "2.31.0", "requests-2.31.0.tar.gz", []byte("sdist")).Body.Close()

	// HTML index.
	htmlResp := get(t, client, srv.URL+"/simple/requests/", "")
	defer htmlResp.Body.Close()
	if ct := htmlResp.Header.Get("Content-Type"); !strings.HasPrefix(ct, contentTypeHTML) {
		t.Errorf("html content-type = %q", ct)
	}
	htmlBody, _ := io.ReadAll(htmlResp.Body)
	for _, want := range []string{"requests-2.31.0-py3-none-any.whl", "requests-2.31.0.tar.gz", "#sha256="} {
		if !strings.Contains(string(htmlBody), want) {
			t.Errorf("html index missing %q:\n%s", want, htmlBody)
		}
	}

	// JSON (PEP 691) index.
	jsonResp := get(t, client, srv.URL+"/simple/requests/", contentTypeJSONv1)
	defer jsonResp.Body.Close()
	if ct := jsonResp.Header.Get("Content-Type"); ct != contentTypeJSONv1 {
		t.Errorf("json content-type = %q, want %q", ct, contentTypeJSONv1)
	}
	jsonBody, _ := io.ReadAll(jsonResp.Body)
	if !strings.Contains(string(jsonBody), `"api-version"`) || !strings.Contains(string(jsonBody), `"sha256"`) {
		t.Errorf("json index unexpected:\n%s", jsonBody)
	}
}

func TestTopLevelIndex(t *testing.T) {
	t.Parallel()
	h := NewHandler(newStore(t), nil)
	srv, client := serve(t, h)

	upload(t, srv, client, "requests", "2.31.0", "requests-2.31.0.tar.gz", []byte("a")).Body.Close()
	upload(t, srv, client, "Flask", "3.0.0", "flask-3.0.0.tar.gz", []byte("b")).Body.Close()

	resp := get(t, client, srv.URL+"/simple/", contentTypeJSONv1)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	for _, want := range []string{`"requests"`, `"flask"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("top-level index missing %q:\n%s", want, body)
		}
	}
}

func TestDownloadMissNoUpstream(t *testing.T) {
	t.Parallel()
	h := NewHandler(newStore(t), nil)
	srv, client := serve(t, h)

	resp := get(t, client, srv.URL+"/packages/ghost/1.0/ghost-1.0.tar.gz", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestProjectIndexUnknown404(t *testing.T) {
	t.Parallel()
	h := NewHandler(newStore(t), nil)
	srv, client := serve(t, h)

	resp := get(t, client, srv.URL+"/simple/ghost/", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestUploadLeadingDotRejected(t *testing.T) {
	t.Parallel()
	h := NewHandler(newStore(t), nil)
	srv, client := serve(t, h)

	// Leading-dot package name.
	resp := upload(t, srv, client, ".evil", "1.0", "evil-1.0.tar.gz", []byte("x"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("leading-dot name status = %d, want 400", resp.StatusCode)
	}

	// Leading-dot filename.
	resp2 := upload(t, srv, client, "good", "1.0", ".hidden.tar.gz", []byte("x"))
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("leading-dot filename status = %d, want 400", resp2.StatusCode)
	}
}

// TestConcurrentUpload uploads a wheel and an sdist for the same new package
// and version at the same time. The disjoint blob paths must both land, and
// the package/version envelopes (which both uploads race to create) must
// converge without a spurious conflict.
func TestConcurrentUpload(t *testing.T) {
	t.Parallel()
	h := NewHandler(newStore(t), nil)
	srv, client := serve(t, h)

	files := map[string][]byte{
		"requests-2.31.0-py3-none-any.whl": []byte("the-wheel"),
		"requests-2.31.0.tar.gz":           []byte("the-sdist"),
	}
	var wg sync.WaitGroup
	statuses := make(chan int, len(files))
	for fn, content := range files {
		wg.Add(1)
		go func(fn string, content []byte) {
			defer wg.Done()
			resp := upload(t, srv, client, "requests", "2.31.0", fn, content)
			statuses <- resp.StatusCode
			resp.Body.Close()
		}(fn, content)
	}
	wg.Wait()
	close(statuses)
	for s := range statuses {
		if s != http.StatusOK {
			t.Errorf("concurrent upload status = %d, want 200", s)
		}
	}

	// Both files must be downloadable with their exact bytes.
	for fn, want := range files {
		resp := get(t, client, srv.URL+"/packages/requests/2.31.0/"+fn, "")
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !bytes.Equal(got, want) {
			t.Errorf("%s body = %q, want %q", fn, got, want)
		}
	}
}

func TestUploadConflict(t *testing.T) {
	t.Parallel()
	h := NewHandler(newStore(t), nil)
	srv, client := serve(t, h)

	first := upload(t, srv, client, "requests", "2.31.0", "requests-2.31.0.tar.gz", []byte("a"))
	first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first upload = %d, want 200", first.StatusCode)
	}
	second := upload(t, srv, client, "requests", "2.31.0", "requests-2.31.0.tar.gz", []byte("b"))
	second.Body.Close()
	if second.StatusCode != http.StatusConflict {
		t.Errorf("re-upload status = %d, want 409", second.StatusCode)
	}
}

// redirectStore decorates a real Store so File.DownloadURL returns a non-empty
// signed-style URL — memblob and fileblob can't sign, so this is the only way
// to exercise the surface's 307 redirect branch against a real backend.
type redirectStore struct {
	core.Store
	signed string
}

func (s redirectStore) Package(name string) core.Package {
	return redirectPkg{Package: s.Store.Package(name), signed: s.signed}
}

type redirectPkg struct {
	core.Package
	signed string
}

func (p redirectPkg) Version(name string) core.Version {
	return redirectVersion{Version: p.Package.Version(name), signed: p.signed}
}

type redirectVersion struct {
	core.Version
	signed string
}

func (v redirectVersion) File(name string) core.File {
	return redirectFile{File: v.Version.File(name), signed: v.signed}
}

func (v redirectVersion) AddFile(ctx context.Context, name string, body io.Reader, opts ...core.CreateOption) (core.File, error) {
	f, err := v.Version.AddFile(ctx, name, body, opts...)
	if err != nil {
		return nil, err
	}
	return redirectFile{File: f, signed: v.signed}, nil
}

type redirectFile struct {
	core.File
	signed string
}

func (f redirectFile) DownloadURL(ctx context.Context) (string, error) {
	return f.signed + "/" + f.File.Name(), nil
}

func TestDownloadRedirect(t *testing.T) {
	t.Parallel()
	base := newStore(t)
	store := redirectStore{Store: base, signed: "https://signed.example/blob"}
	h := NewHandler(store, nil)
	srv, client := serve(t, h)

	upload(t, srv, client, "requests", "2.31.0", "requests-2.31.0.tar.gz", []byte("bytes")).Body.Close()

	resp := get(t, client, srv.URL+"/packages/requests/2.31.0/requests-2.31.0.tar.gz", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want 307", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "https://signed.example/blob/requests-2.31.0.tar.gz" {
		t.Errorf("Location = %q", loc)
	}
}

// TestCacheThrough proves a local miss is filled from the upstream, served to
// the client, and persisted into the Store so the next read is local.
func TestCacheThrough(t *testing.T) {
	t.Parallel()
	fu := newFakeUpstream(t)
	fu.files["requests-2.31.0.tar.gz"] = []byte("upstream-sdist-bytes")
	up, err := NewUpstreamClient(fu.URL)
	if err != nil {
		t.Fatalf("NewUpstreamClient: %v", err)
	}
	store := newStore(t)
	h := NewHandler(store, up)
	srv, client := serve(t, h)

	// File is not local yet — the handler must fill it from upstream.
	resp := get(t, client, srv.URL+"/packages/requests/2.31.0/requests-2.31.0.tar.gz", "")
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if string(got) != "upstream-sdist-bytes" {
		t.Errorf("served body = %q, want upstream bytes", got)
	}

	// The Store must now hold the cached copy.
	f := store.Package("requests").Version("2.31.0").File("requests-2.31.0.tar.gz")
	exists, err := f.Exists(t.Context())
	if err != nil || !exists {
		t.Fatalf("cache-through did not persist file: exists=%v err=%v", exists, err)
	}
	rc, err := f.Read(t.Context())
	if err != nil {
		t.Fatalf("read cached file: %v", err)
	}
	cached, _ := io.ReadAll(rc)
	rc.Close()
	if string(cached) != "upstream-sdist-bytes" {
		t.Errorf("cached body = %q, want upstream bytes", cached)
	}
}

// TestProjectIndexMergesUpstream proves the served index includes the
// upstream's files (rewritten to local download URLs) even when nothing is
// stored locally.
func TestProjectIndexMergesUpstream(t *testing.T) {
	t.Parallel()
	fu := newFakeUpstream(t)
	up, _ := NewUpstreamClient(fu.URL)
	h := NewHandler(newStore(t), up)
	srv, client := serve(t, h)

	resp := get(t, client, srv.URL+"/simple/requests/", "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	for _, want := range []string{
		`/packages/requests/2.31.0/requests-2.31.0-py3-none-any.whl`,
		`/packages/requests/2.31.0/requests-2.31.0.tar.gz`,
		`#sha256=deadbeef`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("merged index missing %q:\n%s", want, body)
		}
	}
}

// TestCacheThroughMissOn404 maps an upstream not-found to a 404.
func TestCacheThroughMissOn404(t *testing.T) {
	t.Parallel()
	fu := newFakeUpstream(t)
	up, _ := NewUpstreamClient(fu.URL)
	h := NewHandler(newStore(t), up)
	srv, client := serve(t, h)

	resp := get(t, client, srv.URL+"/packages/ghost/1.0/ghost-1.0.tar.gz", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}
