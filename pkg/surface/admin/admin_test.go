package admin

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gocloud.dev/blob"
	"gocloud.dev/blob/memblob"

	"github.com/yolocs/open-artifact/pkg/logging"
	"github.com/yolocs/open-artifact/pkg/namespace"
)

func newServer(t *testing.T) (*httptest.Server, *blob.Bucket) {
	t.Helper()
	b := memblob.OpenBucket(nil)
	t.Cleanup(func() { b.Close() })
	store, err := namespace.NewStore(b, "")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	logger, err := logging.New(io.Discard, logging.Options{Level: "error", Format: "text"})
	if err != nil {
		t.Fatalf("logging.New: %v", err)
	}
	srv := httptest.NewServer(Handler(store, logger))
	t.Cleanup(srv.Close)
	return srv, b
}

func do(t *testing.T, srv *httptest.Server, method, path, body string) *http.Response {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, r)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("Do %s %s: %v", method, path, err)
	}
	return resp
}

func decode(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode body: %v", err)
	}
}

func TestAdminCreateUpdateGetDelete(t *testing.T) {
	t.Parallel()
	srv, _ := newServer(t)

	// Create -> 201.
	resp := do(t, srv, http.MethodPut, "/admin/v1/namespaces/team-a", `{"mode":"proxy","proxy":{"upstream":"https://pypi.org/simple"}}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	var created namespace.Namespace
	decode(t, resp, &created)
	if created.Name != "team-a" || !created.Spec.IsProxy() || created.Spec.SchemaVersion != 1 {
		t.Errorf("create body = %+v, want normalized proxy namespace", created)
	}

	// Update (same name) -> 200.
	resp = do(t, srv, http.MethodPut, "/admin/v1/namespaces/team-a", `{}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// Get -> 200, now hosted (empty mode).
	resp = do(t, srv, http.MethodGet, "/admin/v1/namespaces/team-a", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, want 200", resp.StatusCode)
	}
	var got namespace.Namespace
	decode(t, resp, &got)
	if got.Spec.Mode != "" {
		t.Errorf("get mode = %q, want empty (hosted)", got.Spec.Mode)
	}

	// Delete -> 204.
	resp = do(t, srv, http.MethodDelete, "/admin/v1/namespaces/team-a", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()

	// Get after delete -> 404.
	resp = do(t, srv, http.MethodGet, "/admin/v1/namespaces/team-a", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get-after-delete status = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAdminList(t *testing.T) {
	t.Parallel()
	srv, _ := newServer(t)

	for _, name := range []string{"zeta", "alpha"} {
		resp := do(t, srv, http.MethodPut, "/admin/v1/namespaces/"+name, `{}`)
		resp.Body.Close()
	}

	resp := do(t, srv, http.MethodGet, "/admin/v1/namespaces", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Namespaces []namespace.Namespace `json:"namespaces"`
	}
	decode(t, resp, &body)
	if len(body.Namespaces) != 2 || body.Namespaces[0].Name != "alpha" || body.Namespaces[1].Name != "zeta" {
		t.Errorf("list = %+v, want sorted [alpha zeta]", body.Namespaces)
	}
}

func TestAdminErrorMapping(t *testing.T) {
	t.Parallel()
	srv, _ := newServer(t)

	cases := []struct {
		name   string
		method string
		path   string
		body   string
		want   int
	}{
		{name: "invalid name", method: http.MethodPut, path: "/admin/v1/namespaces/Bad_Name", body: `{}`, want: http.StatusBadRequest},
		{name: "reserved name", method: http.MethodPut, path: "/admin/v1/namespaces/admin", body: `{}`, want: http.StatusBadRequest},
		{name: "invalid proxy", method: http.MethodPut, path: "/admin/v1/namespaces/team-a", body: `{"mode":"proxy"}`, want: http.StatusBadRequest},
		{name: "hosted with proxy", method: http.MethodPut, path: "/admin/v1/namespaces/team-a", body: `{"proxy":{"upstream":"https://x"}}`, want: http.StatusBadRequest},
		{name: "unsupported schema", method: http.MethodPut, path: "/admin/v1/namespaces/team-a", body: `{"schema_version":99}`, want: http.StatusBadRequest},
		{name: "malformed json", method: http.MethodPut, path: "/admin/v1/namespaces/team-a", body: `{not json`, want: http.StatusBadRequest},
		{name: "unknown field", method: http.MethodPut, path: "/admin/v1/namespaces/team-a", body: `{"bogus":1}`, want: http.StatusBadRequest},
		{name: "get missing", method: http.MethodGet, path: "/admin/v1/namespaces/ghost", want: http.StatusNotFound},
		{name: "delete missing", method: http.MethodDelete, path: "/admin/v1/namespaces/ghost", want: http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := do(t, srv, tc.method, tc.path, tc.body)
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
			if resp.StatusCode >= 400 {
				var e errorResponse
				if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
					t.Fatalf("decode error body: %v", err)
				}
				if e.Error == "" {
					t.Errorf("error body missing message")
				}
			}
		})
	}
}

func TestAdminDeleteConflict(t *testing.T) {
	t.Parallel()
	srv, b := newServer(t)

	resp := do(t, srv, http.MethodPut, "/admin/v1/namespaces/team-a", `{}`)
	resp.Body.Close()

	// Lay down real package data through the data-plane factory over the same
	// bucket; a non-empty namespace must refuse deletion with 409.
	catalog, err := namespace.NewStore(b, "")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	reg, err := namespace.NewRegistry(b, "", catalog)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	scoped, err := reg.For("team-a", "pypi")
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	ds, err := scoped.Store()
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if _, err := ds.Package("requests").Version("2.31.0").AddFile(t.Context(), "requests-2.31.0.whl", bytes.NewReader([]byte("x"))); err != nil {
		t.Fatalf("AddFile: %v", err)
	}

	resp = do(t, srv, http.MethodDelete, "/admin/v1/namespaces/team-a", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete non-empty status = %d, want 409", resp.StatusCode)
	}
}
