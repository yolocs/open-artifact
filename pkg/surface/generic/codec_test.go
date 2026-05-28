package generic

import (
	"crypto"
	"net/http"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/yolocs/open-artifact/pkg/core"
)

func TestDownloadContentType(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		file string
		meta core.Meta
		want string
	}{
		{
			name: "annotation wins",
			file: "thing.bin",
			meta: core.Meta{Annotations: map[string]any{annContentType: "application/custom"}},
			want: "application/custom",
		},
		{
			name: "extension fallback",
			file: "notes.json",
			meta: core.Meta{},
			want: "application/json",
		},
		{
			name: "octet-stream default",
			file: "no-extension",
			meta: core.Meta{},
			want: "application/octet-stream",
		},
		{
			name: "empty annotation falls through to extension",
			file: "page.css",
			meta: core.Meta{Annotations: map[string]any{annContentType: ""}},
			want: "text/css; charset=utf-8",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := downloadContentType(tc.file, tc.meta); got != tc.want {
				t.Errorf("downloadContentType(%q) = %q, want %q", tc.file, got, tc.want)
			}
		})
	}
}

func TestParseChecksumHeaders(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		headers map[string]string
		want    []core.ExpectedDigest
		wantErr bool
	}{
		{
			name:    "none",
			headers: nil,
			want:    nil,
		},
		{
			name:    "sha256",
			headers: map[string]string{"X-Checksum-Sha256": "abcd"},
			want:    []core.ExpectedDigest{{Hash: crypto.SHA256, Sum: []byte{0xab, 0xcd}}},
		},
		{
			name: "all three preserve order",
			headers: map[string]string{
				"X-Checksum-Sha256": "00",
				"X-Checksum-Sha1":   "11",
				"X-Checksum-Sha512": "22",
			},
			want: []core.ExpectedDigest{
				{Hash: crypto.SHA256, Sum: []byte{0x00}},
				{Hash: crypto.SHA1, Sum: []byte{0x11}},
				{Hash: crypto.SHA512, Sum: []byte{0x22}},
			},
		},
		{
			name:    "non-hex is rejected",
			headers: map[string]string{"X-Checksum-Sha256": "zz"},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := http.Header{}
			for k, v := range tc.headers {
				h.Set(k, v)
			}
			got, err := parseChecksumHeaders(h)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseChecksumHeaders() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseChecksumHeaders() error = %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("parseChecksumHeaders() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestTimePtr(t *testing.T) {
	t.Parallel()

	if got := timePtr(time.Time{}); got != nil {
		t.Errorf("timePtr(zero) = %v, want nil", got)
	}
	now := time.Now()
	if got := timePtr(now); got == nil || !got.Equal(now) {
		t.Errorf("timePtr(now) = %v, want %v", got, now)
	}
}
