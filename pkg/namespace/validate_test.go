package namespace

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/yolocs/open-artifact/pkg/proxy/filter"
)

func TestValidateName(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{name: "simple", in: "team-a"},
		{name: "single char", in: "a"},
		{name: "digits", in: "team123"},
		{name: "max length", in: strings.Repeat("a", 64)},
		{name: "empty", in: "", wantErr: true},
		{name: "too long", in: strings.Repeat("a", 65), wantErr: true},
		{name: "uppercase", in: "TeamA", wantErr: true},
		{name: "underscore", in: "team_a", wantErr: true},
		{name: "leading underscore", in: "_team", wantErr: true},
		{name: "leading dot", in: ".team", wantErr: true},
		{name: "leading dash", in: "-team", wantErr: true},
		{name: "trailing dash", in: "team-", wantErr: true},
		{name: "slash", in: "team/a", wantErr: true},
		{name: "space", in: "team a", wantErr: true},
		{name: "reserved admin", in: "admin", wantErr: true},
		{name: "reserved healthz", in: "healthz", wantErr: true},
		{name: "reserved readyz", in: "readyz", wantErr: true},
		{name: "reserved metrics", in: "metrics", wantErr: true},
		{name: "reserved simple", in: "simple", wantErr: true},
		{name: "reserved maven2", in: "maven2", wantErr: true},
		{name: "reserved v2", in: "v2", wantErr: true},
		{name: "reserved npm", in: "npm", wantErr: true},
		{name: "reserved pypi", in: "pypi", wantErr: true},
		{name: "reserved open-artifact", in: "open-artifact", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateName(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateName(%q) error = %v, wantErr = %v", tc.in, err, tc.wantErr)
			}
			if tc.wantErr && !errors.Is(err, ErrInvalidName) {
				t.Errorf("ValidateName(%q) error = %v, want errors.Is ErrInvalidName", tc.in, err)
			}
		})
	}
}

func TestNormalizeForWrite(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		in      Spec
		want    Spec
		wantErr error
	}{
		{
			name: "empty mode stays hosted with stamped version",
			in:   Spec{},
			want: Spec{SchemaVersion: 1},
		},
		{
			name: "explicit hosted normalized to empty",
			in:   Spec{Mode: ModeHosted},
			want: Spec{SchemaVersion: 1, Mode: ""},
		},
		{
			name: "proxy with absolute https upstream",
			in:   Spec{Mode: ModeProxy, Proxy: Proxy{Upstream: "https://pypi.org/simple"}},
			want: Spec{SchemaVersion: 1, Mode: ModeProxy, Proxy: Proxy{Upstream: "https://pypi.org/simple"}},
		},
		{
			name: "proxy with http upstream",
			in:   Spec{Mode: ModeProxy, Proxy: Proxy{Upstream: "http://registry.local"}},
			want: Spec{SchemaVersion: 1, Mode: ModeProxy, Proxy: Proxy{Upstream: "http://registry.local"}},
		},
		{
			name: "version below current accepted and stamped",
			in:   Spec{SchemaVersion: 1, Mode: ModeProxy, Proxy: Proxy{Upstream: "https://example.com"}},
			want: Spec{SchemaVersion: 1, Mode: ModeProxy, Proxy: Proxy{Upstream: "https://example.com"}},
		},
		{
			name:    "schema version too new",
			in:      Spec{SchemaVersion: 2},
			wantErr: ErrUnsupportedSchemaVersion,
		},
		{
			name:    "hosted with proxy block rejected",
			in:      Spec{Proxy: Proxy{Upstream: "https://pypi.org"}},
			wantErr: ErrInvalidProxy,
		},
		{
			name:    "explicit hosted with proxy block rejected",
			in:      Spec{Mode: ModeHosted, Proxy: Proxy{Upstream: "https://pypi.org"}},
			wantErr: ErrInvalidProxy,
		},
		{
			name:    "hosted with filters rejected",
			in:      Spec{Proxy: Proxy{Filters: []filter.Spec{{Kind: filter.KindAllow, Patterns: []string{"*"}}}}},
			wantErr: ErrInvalidProxy,
		},
		{
			name: "proxy with valid filters",
			in:   Spec{Mode: ModeProxy, Proxy: Proxy{Upstream: "https://example.com", Filters: []filter.Spec{{Kind: filter.KindAllow, Patterns: []string{"@myorg/*"}}, {Kind: filter.KindDelay, MinAge: "24h"}}}},
			want: Spec{SchemaVersion: 1, Mode: ModeProxy, Proxy: Proxy{Upstream: "https://example.com", Filters: []filter.Spec{{Kind: filter.KindAllow, Patterns: []string{"@myorg/*"}}, {Kind: filter.KindDelay, MinAge: "24h"}}}},
		},
		{
			name:    "proxy with invalid filter rejected",
			in:      Spec{Mode: ModeProxy, Proxy: Proxy{Upstream: "https://example.com", Filters: []filter.Spec{{Kind: "mirror"}}}},
			wantErr: ErrInvalidProxy,
		},
		{
			name:    "proxy missing upstream",
			in:      Spec{Mode: ModeProxy},
			wantErr: ErrInvalidProxy,
		},
		{
			name:    "proxy relative upstream",
			in:      Spec{Mode: ModeProxy, Proxy: Proxy{Upstream: "pypi.org/simple"}},
			wantErr: ErrInvalidProxy,
		},
		{
			name:    "proxy non-http scheme",
			in:      Spec{Mode: ModeProxy, Proxy: Proxy{Upstream: "ftp://pypi.org"}},
			wantErr: ErrInvalidProxy,
		},
		{
			name:    "proxy no host",
			in:      Spec{Mode: ModeProxy, Proxy: Proxy{Upstream: "https://"}},
			wantErr: ErrInvalidProxy,
		},
		{
			name:    "unknown mode",
			in:      Spec{Mode: "mirror"},
			wantErr: ErrInvalidProxy,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := normalizeForWrite(tc.in)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("normalizeForWrite() error = %v, want errors.Is %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeForWrite() unexpected error: %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("normalizeForWrite() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestNormalizeForWritePreservesFormatMap(t *testing.T) {
	t.Parallel()

	in := Spec{Format: map[string]any{"unknown": "value", "nested": map[string]any{"a": float64(1)}}}
	got, err := normalizeForWrite(in)
	if err != nil {
		t.Fatalf("normalizeForWrite() error: %v", err)
	}
	if diff := cmp.Diff(in.Format, got.Format); diff != "" {
		t.Errorf("format map not round-tripped (-want +got):\n%s", diff)
	}
}

func TestNormalizeForRead(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		in      Spec
		want    Spec
		wantErr error
	}{
		{
			name: "missing version defaults to current",
			in:   Spec{},
			want: Spec{SchemaVersion: 1},
		},
		{
			name: "current version kept",
			in:   Spec{SchemaVersion: 1, Mode: ModeProxy},
			want: Spec{SchemaVersion: 1, Mode: ModeProxy},
		},
		{
			name:    "future version rejected",
			in:      Spec{SchemaVersion: 99},
			wantErr: ErrUnsupportedSchemaVersion,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := normalizeForRead(tc.in)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("normalizeForRead() error = %v, want errors.Is %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeForRead() unexpected error: %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("normalizeForRead() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
