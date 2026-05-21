package command

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestValidateBucketPrefix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		prefix  string
		wantErr bool
	}{
		{name: "empty ok", prefix: ""},
		{name: "single segment", prefix: "team-a"},
		{name: "nested segments", prefix: "team-a/pypi"},
		{name: "absolute rejected", prefix: "/team-a", wantErr: true},
		{name: "trailing slash empty segment", prefix: "team-a/", wantErr: true},
		{name: "leading slash empty segment", prefix: "/", wantErr: true},
		{name: "double slash empty segment", prefix: "a//b", wantErr: true},
		{name: "dotdot rejected", prefix: "a/../b", wantErr: true},
		{name: "leading dot segment rejected", prefix: ".hidden", wantErr: true},
		{name: "nested leading dot rejected", prefix: "a/.meta", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateBucketPrefix(tc.prefix)
			if tc.wantErr != (err != nil) {
				t.Fatalf("validateBucketPrefix(%q) err = %v, wantErr = %v", tc.prefix, err, tc.wantErr)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()

	base := func() *runtimeConfig {
		return &runtimeConfig{
			Port:        8080,
			BucketURL:   "mem://",
			MetricsPath: "/metrics",
			LogLevel:    "info",
			LogFormat:   "text",
			AuthnKind:   "oidc",
		}
	}

	tests := []struct {
		name      string
		mutate    func(*runtimeConfig)
		dataPlane bool
		wantErr   bool
	}{
		{name: "valid data plane", dataPlane: true},
		{name: "valid admin plane", dataPlane: false},
		{name: "missing bucket url", mutate: func(c *runtimeConfig) { c.BucketURL = "" }, wantErr: true},
		{name: "bad prefix", mutate: func(c *runtimeConfig) { c.BucketPrefix = "../escape" }, wantErr: true},
		{name: "bad log level", mutate: func(c *runtimeConfig) { c.LogLevel = "loud" }, wantErr: true},
		{name: "bad log format", mutate: func(c *runtimeConfig) { c.LogFormat = "xml" }, wantErr: true},
		{name: "bad metrics path", mutate: func(c *runtimeConfig) { c.MetricsPath = "metrics" }, wantErr: true},
		{name: "port too low", mutate: func(c *runtimeConfig) { c.Port = 0 }, wantErr: true},
		{name: "port too high", mutate: func(c *runtimeConfig) { c.Port = 70000 }, wantErr: true},
		{
			name:      "unsupported repo type",
			dataPlane: true,
			mutate:    func(c *runtimeConfig) { c.RepoType = "rubygems" },
			wantErr:   true,
		},
		{
			name:      "valid repo type",
			dataPlane: true,
			mutate:    func(c *runtimeConfig) { c.RepoType = "pypi" },
		},
		{
			name:      "repo type ignored on admin plane",
			dataPlane: false,
			mutate:    func(c *runtimeConfig) { c.RepoType = "rubygems" },
		},
		{
			name:      "unsupported authn kind",
			dataPlane: true,
			mutate:    func(c *runtimeConfig) { c.AuthnKind = "mtls" },
			wantErr:   true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := base()
			if tc.mutate != nil {
				tc.mutate(cfg)
			}
			err := cfg.validate(tc.dataPlane)
			if tc.wantErr != (err != nil) {
				t.Fatalf("validate() err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func TestValidateJoinsErrors(t *testing.T) {
	t.Parallel()

	cfg := &runtimeConfig{Port: 0, BucketURL: "", MetricsPath: "x", LogLevel: "x", LogFormat: "x"}
	err := cfg.validate(false)
	if err == nil {
		t.Fatal("validate() = nil, want joined errors")
	}
	// errors.Join renders one error per line; expect several.
	got := err.Error()
	for _, want := range []string{"--port", "--bucket-url", "--metrics-path", "--log-level", "--log-format"} {
		if !strings.Contains(got, want) {
			t.Errorf("joined error missing %q; got: %s", want, got)
		}
	}
}

func TestSplitCSV(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{name: "nil", in: nil, want: nil},
		{name: "already split", in: []string{"a", "b"}, want: []string{"a", "b"}},
		{name: "single comma joined", in: []string{"a,b,c"}, want: []string{"a", "b", "c"}},
		{name: "trims and drops empties", in: []string{" a , ,b "}, want: []string{"a", "b"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := splitCSV(tc.in)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("splitCSV() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
