package surface

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWrapWithFormatAndSetOperation(t *testing.T) {
	t.Parallel()

	var gotFormat, gotOp string
	h := WrapWithFormat("pypi")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		SetOperation(r, "pypi_upload")
		gotFormat = Format(r)
		gotOp = Operation(r)
	}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/upload", nil))

	if gotFormat != "pypi" {
		t.Fatalf("format = %q, want pypi", gotFormat)
	}
	if gotOp != "pypi_upload" {
		t.Fatalf("operation = %q, want pypi_upload", gotOp)
	}
}

func TestOperationFallsBackFromMethod(t *testing.T) {
	t.Parallel()

	tests := []struct {
		method string
		want   string
	}{
		{method: http.MethodGet, want: "read"},
		{method: http.MethodHead, want: "read"},
		{method: http.MethodPost, want: "write"},
		{method: http.MethodPatch, want: "write"},
		{method: http.MethodDelete, want: "delete"},
		{method: "PROPFIND", want: "propfind"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.method, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(tt.method, "/", nil)
			if got := Operation(req); got != tt.want {
				t.Fatalf("Operation(%s) = %q, want %q", tt.method, got, tt.want)
			}
		})
	}
}
