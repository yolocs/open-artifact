package surface

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yolocs/open-artifact/pkg/core"
)

func TestWriteStoreError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		err        error
		wantWrote  bool
		wantStatus int
	}{
		{name: "nil passes through", err: nil, wantWrote: false},
		{name: "not found", err: core.ErrNotFound, wantWrote: true, wantStatus: http.StatusNotFound},
		{name: "already exists", err: core.ErrAlreadyExists, wantWrote: true, wantStatus: http.StatusConflict},
		{name: "digest mismatch", err: core.ErrDigestMismatch, wantWrote: true, wantStatus: http.StatusUnprocessableEntity},
		{name: "unsupported", err: core.ErrUnsupported, wantWrote: true, wantStatus: http.StatusNotImplemented},
		{name: "unknown", err: errors.New("boom"), wantWrote: true, wantStatus: http.StatusInternalServerError},
		{name: "wrapped not found", err: fmt.Errorf("read %q: %w", "x", core.ErrNotFound), wantWrote: true, wantStatus: http.StatusNotFound},
		{name: "wrapped already exists", err: fmt.Errorf("probe: %w", core.ErrAlreadyExists), wantWrote: true, wantStatus: http.StatusConflict},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			got := WriteStoreError(rec, tc.err)
			if got != tc.wantWrote {
				t.Fatalf("WriteStoreError returned %v, want %v", got, tc.wantWrote)
			}
			if !tc.wantWrote {
				if rec.Code != http.StatusOK {
					t.Errorf("expected no write, got status %d", rec.Code)
				}
				return
			}
			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
		})
	}
}

// TestWriteStoreErrorHidesDetail ensures internal error text never reaches the
// client — only the generic status message does.
func TestWriteStoreErrorHidesDetail(t *testing.T) {
	t.Parallel()
	secret := "blobstore: open s3://internal-bucket/secret: permission denied"
	rec := httptest.NewRecorder()
	WriteStoreError(rec, errors.New(secret))
	if body := rec.Body.String(); strings.Contains(body, "internal-bucket") || strings.Contains(body, "permission denied") {
		t.Errorf("response body leaked internal detail: %q", body)
	}
}
