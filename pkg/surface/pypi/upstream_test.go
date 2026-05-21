package pypi

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// fakeUpstream is an httptest server that speaks the PEP 691 JSON simple API
// and serves file bytes, standing in for pypi.org in tests.
type fakeUpstream struct {
	*httptest.Server
	// files maps filename -> bytes the /files/ endpoint serves.
	files map[string][]byte
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	fu := &fakeUpstream{files: map[string][]byte{}}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /simple/{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentTypeJSONv1)
		_, _ = io.WriteString(w, `{"meta":{"api-version":"1.0"},"projects":[{"name":"requests"},{"name":"Flask"}]}`)
	})

	mux.HandleFunc("GET /simple/{package}/{$}", func(w http.ResponseWriter, r *http.Request) {
		pkg := r.PathValue("package")
		if pkg != "requests" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", contentTypeJSONv1)
		// Use a relative file URL to exercise URL resolution.
		_, _ = io.WriteString(w, `{
			"meta":{"api-version":"1.0"},
			"name":"requests",
			"files":[
				{"filename":"requests-2.31.0-py3-none-any.whl","url":"../../files/requests-2.31.0-py3-none-any.whl","hashes":{"sha256":"deadbeef"}},
				{"filename":"requests-2.31.0.tar.gz","url":"../../files/requests-2.31.0.tar.gz","hashes":{"sha256":"cafef00d"}}
			]
		}`)
	})

	mux.HandleFunc("GET /files/{filename}", func(w http.ResponseWriter, r *http.Request) {
		b, ok := fu.files[r.PathValue("filename")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(b)
	})

	fu.Server = httptest.NewServer(mux)
	t.Cleanup(fu.Close)
	return fu
}

func TestUpstreamProject(t *testing.T) {
	t.Parallel()
	fu := newFakeUpstream(t)
	c, err := NewUpstreamClient(fu.URL)
	if err != nil {
		t.Fatalf("NewUpstreamClient: %v", err)
	}

	proj, err := c.Project(t.Context(), "requests")
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	want := &ProjectIndex{
		Name: "requests",
		Files: []UpstreamFile{
			{Filename: "requests-2.31.0-py3-none-any.whl", URL: fu.URL + "/files/requests-2.31.0-py3-none-any.whl", Hashes: map[string]string{"sha256": "deadbeef"}},
			{Filename: "requests-2.31.0.tar.gz", URL: fu.URL + "/files/requests-2.31.0.tar.gz", Hashes: map[string]string{"sha256": "cafef00d"}},
		},
	}
	if diff := cmp.Diff(want, proj); diff != "" {
		t.Errorf("Project mismatch (-want +got):\n%s", diff)
	}
}

func TestUpstreamProjectNotFound(t *testing.T) {
	t.Parallel()
	fu := newFakeUpstream(t)
	c, _ := NewUpstreamClient(fu.URL)
	_, err := c.Project(t.Context(), "nonexistent")
	if !errors.Is(err, ErrUpstreamNotFound) {
		t.Fatalf("Project: got %v, want ErrUpstreamNotFound", err)
	}
}

func TestUpstreamTopLevel(t *testing.T) {
	t.Parallel()
	fu := newFakeUpstream(t)
	c, _ := NewUpstreamClient(fu.URL)
	names, err := c.TopLevel(t.Context())
	if err != nil {
		t.Fatalf("TopLevel: %v", err)
	}
	if diff := cmp.Diff([]string{"requests", "Flask"}, names); diff != "" {
		t.Errorf("TopLevel mismatch (-want +got):\n%s", diff)
	}
}

func TestUpstreamFetchFile(t *testing.T) {
	t.Parallel()
	fu := newFakeUpstream(t)
	fu.files["requests-2.31.0.tar.gz"] = []byte("tarball-bytes")
	c, _ := NewUpstreamClient(fu.URL)

	fr, err := c.FetchFile(t.Context(), fu.URL+"/files/requests-2.31.0.tar.gz")
	if err != nil {
		t.Fatalf("FetchFile: %v", err)
	}
	defer fr.Body.Close()
	got, _ := io.ReadAll(fr.Body)
	if string(got) != "tarball-bytes" {
		t.Errorf("FetchFile body = %q, want %q", got, "tarball-bytes")
	}
}

func TestUpstreamFetchFileNotFound(t *testing.T) {
	t.Parallel()
	fu := newFakeUpstream(t)
	c, _ := NewUpstreamClient(fu.URL)
	_, err := c.FetchFile(t.Context(), fu.URL+"/files/missing.tar.gz")
	if !errors.Is(err, ErrUpstreamNotFound) {
		t.Fatalf("FetchFile: got %v, want ErrUpstreamNotFound", err)
	}
}

func TestNewUpstreamClientValidation(t *testing.T) {
	t.Parallel()
	cases := []string{"", "not a url", "ftp://x", "/relative"}
	for _, in := range cases {
		in := in
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			if _, err := NewUpstreamClient(in); err == nil {
				t.Errorf("NewUpstreamClient(%q) = nil error, want error", in)
			}
		})
	}
}
