package surface

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/yolocs/open-artifact/pkg/auth"
	"github.com/yolocs/open-artifact/pkg/core"
	"github.com/yolocs/open-artifact/pkg/namespace"
)

func TestWriteStoreErrorMapsCoreSentinels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		err    error
		status int
		body   string
	}{
		{name: "not found", err: core.ErrNotFound, status: http.StatusNotFound, body: `{"error":"not found"}` + "\n"},
		{name: "already exists", err: core.ErrAlreadyExists, status: http.StatusConflict, body: `{"error":"already exists"}` + "\n"},
		{name: "digest mismatch", err: core.ErrDigestMismatch, status: http.StatusUnprocessableEntity, body: `{"error":"digest mismatch"}` + "\n"},
		{name: "unsupported", err: core.ErrUnsupported, status: http.StatusNotImplemented, body: `{"error":"unsupported"}` + "\n"},
		{name: "wrapped", err: errors.Join(errors.New("outer"), core.ErrNotFound), status: http.StatusNotFound, body: `{"error":"not found"}` + "\n"},
		{name: "unknown", err: errors.New("database password leaked"), status: http.StatusInternalServerError, body: `{"error":"internal server error"}` + "\n"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/x", nil)

			WriteStoreError(rr, req, tt.err)

			if rr.Code != tt.status {
				t.Fatalf("status = %d, want %d", rr.Code, tt.status)
			}
			if got := rr.Body.String(); got != tt.body {
				t.Fatalf("body mismatch (-want +got):\n%s", cmp.Diff(tt.body, got))
			}
			if got := rr.Header().Get("Content-Type"); got != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", got)
			}
		})
	}
}

func TestWriteNamespaceErrorMapsNamespaceAndAuthErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		err    error
		op     NamespaceErrorContext
		status int
		body   string
	}{
		{name: "invalid name", err: namespace.ErrInvalidName, status: http.StatusBadRequest, body: `{"error":"invalid namespace name"}` + "\n"},
		{name: "invalid proxy", err: namespace.ErrInvalidProxy, status: http.StatusBadRequest, body: `{"error":"invalid proxy namespace"}` + "\n"},
		{name: "not found", err: namespace.ErrNotFound, status: http.StatusNotFound, body: `{"error":"namespace not found"}` + "\n"},
		{name: "not empty", err: namespace.ErrNotEmpty, status: http.StatusConflict, body: `{"error":"namespace not empty"}` + "\n"},
		{name: "unauthorized", err: auth.ErrUnauthorized, status: http.StatusForbidden, body: `{"error":"forbidden"}` + "\n"},
		{
			name:   "unsupported schema version during data write",
			err:    namespace.ErrUnsupportedSchemaVersion,
			op:     NamespaceDataWrite,
			status: http.StatusBadRequest,
			body:   `{"error":"unsupported namespace schema version"}` + "\n",
		},
		{
			name:   "unsupported schema version during data read",
			err:    namespace.ErrUnsupportedSchemaVersion,
			op:     NamespaceDataRead,
			status: http.StatusInternalServerError,
			body:   `{"error":"internal server error"}` + "\n",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/x", nil)

			WriteNamespaceError(rr, req, tt.err, tt.op)

			if rr.Code != tt.status {
				t.Fatalf("status = %d, want %d", rr.Code, tt.status)
			}
			if got := rr.Body.String(); got != tt.body {
				t.Fatalf("body mismatch (-want +got):\n%s", cmp.Diff(tt.body, got))
			}
		})
	}
}

func TestMethodNotAllowedSetsAllowHeader(t *testing.T) {
	t.Parallel()

	rr := httptest.NewRecorder()
	WriteMethodNotAllowed(rr, []string{http.MethodGet, http.MethodHead})

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusMethodNotAllowed)
	}
	if got := rr.Header().Get("Allow"); got != "GET, HEAD" {
		t.Fatalf("Allow = %q, want GET, HEAD", got)
	}
	if got := rr.Body.String(); got != `{"error":"method not allowed"}`+"\n" {
		t.Fatalf("body = %q", got)
	}
}

func TestMaxBytesReaderLimitsRequestBody(t *testing.T) {
	t.Parallel()

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader("abcdef"))
	req = WithMaxBody(rr, req, 3)

	_, err := io.ReadAll(req.Body)
	if err == nil {
		t.Fatal("ReadAll succeeded, want max bytes error")
	}
}

func TestHeadAsGetPreservesHeadersAndStatusWithoutBody(t *testing.T) {
	t.Parallel()

	h := HeadAsGet(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("method seen by GET handler = %s, want GET", r.Method)
		}
		w.Header().Set("ETag", `"abc"`)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("payload"))
	}))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodHead, "/artifact", nil)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusCreated)
	}
	if got := rr.Header().Get("ETag"); got != `"abc"` {
		t.Fatalf("ETag = %q, want %q", got, `"abc"`)
	}
	if got := rr.Body.String(); got != "" {
		t.Fatalf("HEAD body = %q, want empty", got)
	}
}

func TestRedirectOrStreamFileRedirectsDownloadURLs(t *testing.T) {
	t.Parallel()

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		method := method
		t.Run(method, func(t *testing.T) {
			t.Parallel()

			rr := httptest.NewRecorder()
			req := httptest.NewRequest(method, "/files/pkg.whl", nil)
			file := &fakeFile{name: "pkg.whl", downloadURL: "https://blob.example/pkg.whl"}

			RedirectOrStreamFile(rr, req, file, "application/octet-stream")

			if rr.Code != http.StatusTemporaryRedirect {
				t.Fatalf("status = %d, want %d", rr.Code, http.StatusTemporaryRedirect)
			}
			if got := rr.Header().Get("Location"); got != file.downloadURL {
				t.Fatalf("Location = %q, want %q", got, file.downloadURL)
			}
			if got := rr.Header().Get("Content-Type"); got != "application/octet-stream" {
				t.Fatalf("Content-Type = %q, want application/octet-stream", got)
			}
			if method == http.MethodHead && rr.Body.Len() != 0 {
				t.Fatalf("HEAD body length = %d, want 0", rr.Body.Len())
			}
		})
	}
}

func TestRedirectOrStreamFileStreamsWhenNoDownloadURL(t *testing.T) {
	t.Parallel()

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/files/pkg.whl", nil)
	file := &fakeFile{name: "pkg.whl", body: []byte("wheel bytes"), digest: "sha256:abc123"}

	RedirectOrStreamFile(rr, req, file, "application/octet-stream")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	if got := rr.Body.String(); got != "wheel bytes" {
		t.Fatalf("body = %q, want wheel bytes", got)
	}
	if got := rr.Header().Get("Content-Length"); got != "11" {
		t.Fatalf("Content-Length = %q, want 11", got)
	}
	if got := rr.Header().Get("ETag"); got != `"sha256:abc123"` {
		t.Fatalf("ETag = %q, want %q", got, `"sha256:abc123"`)
	}
}

func TestRedirectOrStreamFileMapsDigestMismatchBeforeWritingBody(t *testing.T) {
	t.Parallel()

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/files/pkg.whl", nil)
	file := &fakeFile{name: "pkg.whl", body: []byte("corrupt bytes"), readErr: core.ErrDigestMismatch}

	RedirectOrStreamFile(rr, req, file, "application/octet-stream")

	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusUnprocessableEntity)
	}
	if got := rr.Body.String(); got != `{"error":"digest mismatch"}`+"\n" {
		t.Fatalf("body = %q", got)
	}
}

func TestOptionsApplyCommonSurfaceSettings(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	authn := func(next http.Handler) http.Handler { return next }

	opts := NewOptions(WithAuthMiddleware(authn), WithMaxUploadBytes(42), WithLogger(logger))

	if opts.AuthMiddleware == nil {
		t.Fatal("AuthMiddleware nil, want configured middleware")
	}
	if opts.MaxUploadBytes != 42 {
		t.Fatalf("MaxUploadBytes = %d, want 42", opts.MaxUploadBytes)
	}
	if opts.Logger != logger {
		t.Fatalf("Logger mismatch")
	}
}

type fakeFile struct {
	name        string
	downloadURL string
	body        []byte
	readErr     error
	digest      string
}

func (f *fakeFile) Name() string          { return f.name }
func (f *fakeFile) Namespace() string     { return "pypi/global" }
func (f *fakeFile) Version() core.Version { return nil }
func (f *fakeFile) Package() core.Package { return nil }
func (f *fakeFile) Meta(context.Context) (core.Meta, error) {
	return core.Meta{Digest: f.digest, Size: int64(len(f.body))}, nil
}
func (f *fakeFile) Exists(context.Context) (bool, error)        { return true, nil }
func (f *fakeFile) DownloadURL(context.Context) (string, error) { return f.downloadURL, nil }
func (f *fakeFile) Read(context.Context) (io.ReadCloser, error) {
	return &errReadCloser{src: bytes.NewReader(f.body), err: f.readErr}, nil
}

type errReadCloser struct {
	src io.Reader
	err error
}

func (r *errReadCloser) Close() error { return nil }

func (r *errReadCloser) Read(p []byte) (int, error) {
	n, err := r.src.Read(p)
	if err == io.EOF && r.err != nil {
		return n, r.err
	}
	return n, err
}
