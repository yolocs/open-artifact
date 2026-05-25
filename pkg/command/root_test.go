package command

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/yolocs/open-artifact/pkg/logging"
	"github.com/yolocs/open-artifact/pkg/surface/maven"
	"github.com/yolocs/open-artifact/pkg/surface/npm"
	"github.com/yolocs/open-artifact/pkg/surface/pypi"
)

// runServeCapture executes a `serve` command with args, capturing the resolved
// config instead of starting a server. It returns the config (nil if RunE
// failed before run) and any error.
func runServeCapture(t *testing.T, args ...string) (*runtimeConfig, error) {
	t.Helper()
	var captured *runtimeConfig
	cmd := newServeCommand(func(ctx context.Context, cfg *runtimeConfig) error {
		captured = cfg
		// The logger must be on the context by the time run is invoked.
		if logging.FromContext(ctx) == nil {
			t.Error("run invoked without a logger on the context")
		}
		return nil
	})
	cmd.SetArgs(args)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.ExecuteContext(context.Background())
	return captured, err
}

func TestServeDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := runServeCapture(t, "--bucket-url", "mem://",
		"--authn-oidc-issuers", "https://idp.example",
		"--authn-oidc-audience", "open-artifact")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	want := &runtimeConfig{
		Port:              defaultDataPort,
		BucketURL:         "mem://",
		EnableMetrics:     true,
		MetricsPath:       "/metrics",
		LogLevel:          "info",
		LogFormat:         "text",
		AuthnKind:         "oidc",
		AuthnOIDCIssuers:  []string{"https://idp.example"},
		AuthnOIDCAudience: "open-artifact",
		PyPI: pypi.Config{
			MaxUploadBytes:        pypi.DefaultMaxUploadBytes,
			SimpleIndexCacheTTL:   60 * time.Second,
			ProxyIndexCacheTTL:    pypi.DefaultProxyIndexCacheTTL,
			ProxyNegativeCacheTTL: pypi.DefaultProxyNegativeCacheTTL,
		},
		NPM: npm.Config{
			MaxUploadBytes:         npm.DefaultMaxUploadBytes,
			ProxyPackumentMemoTTL:  npm.DefaultProxyPackumentMemoTTL,
			ProxyPackumentCacheTTL: npm.DefaultProxyPackumentCacheTTL,
			ProxyNegativeCacheTTL:  npm.DefaultProxyNegativeCacheTTL,
		},
		Maven: maven.Config{
			MaxUploadBytes: maven.DefaultMaxUploadBytes,
		},
	}
	if diff := cmp.Diff(want, cfg, cmpopts.IgnoreUnexported(runtimeConfig{})); diff != "" {
		t.Errorf("config mismatch (-want +got):\n%s", diff)
	}
}

func TestServeFlagsOverrideDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := runServeCapture(t,
		"--bucket-url", "mem://",
		"--port", "9090",
		"--repo-type", "pypi",
		"--log-level", "debug",
		"--log-format", "json",
		"--enable-metrics=false",
		"--pypi-max-upload-bytes", "1024",
		"--pypi-simple-index-cache-ttl", "30s",
		"--maven-max-upload-bytes", "4096",
		"--authn-oidc-issuers", "https://a.example,https://b.example",
		"--authn-oidc-audience", "open-artifact",
	)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if cfg.Port != 9090 {
		t.Errorf("Port = %d, want 9090", cfg.Port)
	}
	if cfg.RepoType != "pypi" {
		t.Errorf("RepoType = %q, want pypi", cfg.RepoType)
	}
	if cfg.LogLevel != "debug" || cfg.LogFormat != "json" {
		t.Errorf("log level/format = %q/%q, want debug/json", cfg.LogLevel, cfg.LogFormat)
	}
	if cfg.EnableMetrics {
		t.Error("EnableMetrics = true, want false")
	}
	if cfg.PyPI.MaxUploadBytes != 1024 {
		t.Errorf("PyPI.MaxUploadBytes = %d, want 1024", cfg.PyPI.MaxUploadBytes)
	}
	if cfg.PyPI.SimpleIndexCacheTTL != 30*time.Second {
		t.Errorf("PyPI.SimpleIndexCacheTTL = %d, want 30s in nanoseconds", cfg.PyPI.SimpleIndexCacheTTL)
	}
	if cfg.Maven.MaxUploadBytes != 4096 {
		t.Errorf("Maven.MaxUploadBytes = %d, want 4096", cfg.Maven.MaxUploadBytes)
	}
	wantIssuers := []string{"https://a.example", "https://b.example"}
	if diff := cmp.Diff(wantIssuers, cfg.AuthnOIDCIssuers, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("issuers mismatch (-want +got):\n%s", diff)
	}
}

func TestServeEnvResolution(t *testing.T) {
	// Mutates process env; no t.Parallel per the env-var exception.
	t.Setenv("OPEN_ARTIFACT_BUCKET_URL", "mem://")
	t.Setenv("OPEN_ARTIFACT_PORT", "7000")
	t.Setenv("OPEN_ARTIFACT_LOG_FORMAT", "json")
	t.Setenv("OPEN_ARTIFACT_PYPI_MAX_UPLOAD_BYTES", "2048")
	t.Setenv("OPEN_ARTIFACT_PYPI_SIMPLE_INDEX_CACHE_TTL", "15s")
	t.Setenv("OPEN_ARTIFACT_MAVEN_MAX_UPLOAD_BYTES", "8192")
	t.Setenv("OPEN_ARTIFACT_AUTHN_OIDC_ISSUERS", "https://env.example,https://two.example")
	t.Setenv("OPEN_ARTIFACT_AUTHN_OIDC_AUDIENCE", "open-artifact")

	cfg, err := runServeCapture(t)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if cfg.BucketURL != "mem://" {
		t.Errorf("BucketURL = %q, want mem://", cfg.BucketURL)
	}
	if cfg.Port != 7000 {
		t.Errorf("Port = %d, want 7000 (from OPEN_ARTIFACT_PORT)", cfg.Port)
	}
	if cfg.LogFormat != "json" {
		t.Errorf("LogFormat = %q, want json", cfg.LogFormat)
	}
	if cfg.PyPI.MaxUploadBytes != 2048 {
		t.Errorf("PyPI.MaxUploadBytes = %d, want 2048", cfg.PyPI.MaxUploadBytes)
	}
	if cfg.PyPI.SimpleIndexCacheTTL != 15*time.Second {
		t.Errorf("PyPI.SimpleIndexCacheTTL = %d, want 15s in nanoseconds", cfg.PyPI.SimpleIndexCacheTTL)
	}
	if cfg.Maven.MaxUploadBytes != 8192 {
		t.Errorf("Maven.MaxUploadBytes = %d, want 8192", cfg.Maven.MaxUploadBytes)
	}
	wantIssuers := []string{"https://env.example", "https://two.example"}
	if diff := cmp.Diff(wantIssuers, cfg.AuthnOIDCIssuers); diff != "" {
		t.Errorf("issuers from env mismatch (-want +got):\n%s", diff)
	}
}

func TestServeFlagBeatsEnv(t *testing.T) {
	// Mutates process env; no t.Parallel per the env-var exception.
	t.Setenv("OPEN_ARTIFACT_PORT", "7000")

	cfg, err := runServeCapture(t, "--bucket-url", "mem://", "--port", "9999", "--disable-authn")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if cfg.Port != 9999 {
		t.Errorf("Port = %d, want 9999 (flag beats env)", cfg.Port)
	}
}

func TestServePlatformPORT(t *testing.T) {
	// Mutates process env; no t.Parallel per the env-var exception.
	t.Setenv("PORT", "6543")

	cfg, err := runServeCapture(t, "--bucket-url", "mem://", "--disable-authn")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if cfg.Port != 6543 {
		t.Errorf("Port = %d, want 6543 (from platform PORT)", cfg.Port)
	}
}

func TestServeOpenArtifactPortBeatsPlatformPORT(t *testing.T) {
	// Mutates process env; no t.Parallel per the env-var exception.
	t.Setenv("PORT", "6543")
	t.Setenv("OPEN_ARTIFACT_PORT", "6000")

	cfg, err := runServeCapture(t, "--bucket-url", "mem://", "--disable-authn")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if cfg.Port != 6000 {
		t.Errorf("Port = %d, want 6000 (OPEN_ARTIFACT_PORT beats PORT)", cfg.Port)
	}
}

func TestServeValidationError(t *testing.T) {
	t.Parallel()

	if _, err := runServeCapture(t); err == nil {
		t.Fatal("serve without --bucket-url = nil error, want error")
	}
	if _, err := runServeCapture(t, "--bucket-url", "mem://", "--disable-authn", "--repo-type", "rubygems"); err == nil {
		t.Fatal("serve with bad --repo-type = nil error, want error")
	}
}

func TestServeAuthnConfigValidation(t *testing.T) {
	t.Parallel()

	// Neither --disable-authn nor a usable OIDC config: refuse to start.
	if _, err := runServeCapture(t, "--bucket-url", "mem://"); err == nil {
		t.Error("serve without any authn config = nil error, want error")
	}
	// OIDC issuers but no audience: refuse to start.
	if _, err := runServeCapture(t, "--bucket-url", "mem://", "--authn-oidc-issuers", "https://idp"); err == nil {
		t.Error("serve with issuers but no audience = nil error, want error")
	}
	// --disable-authn alone is sufficient.
	if _, err := runServeCapture(t, "--bucket-url", "mem://", "--disable-authn"); err != nil {
		t.Errorf("serve with --disable-authn = %v, want nil", err)
	}
	// --disable-authn and explicit --authn-kind are mutually exclusive.
	if _, err := runServeCapture(t, "--bucket-url", "mem://", "--disable-authn", "--authn-kind", "oidc"); err == nil {
		t.Error("serve with --disable-authn and --authn-kind = nil error, want error")
	}
}

func TestAdminServeDefaultPort(t *testing.T) {
	t.Parallel()

	var captured *runtimeConfig
	cmd := newAdminServeCommand(func(_ context.Context, cfg *runtimeConfig) error {
		captured = cfg
		return nil
	})
	cmd.SetArgs([]string{"--bucket-url", "mem://"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if captured.Port != defaultAdminPort {
		t.Errorf("admin Port = %d, want %d", captured.Port, defaultAdminPort)
	}
}

func TestVersionFlag(t *testing.T) {
	t.Parallel()

	root := NewRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--version"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("execute --version: %v", err)
	}
	if !strings.Contains(out.String(), "open-artifact") {
		t.Errorf("--version output = %q, want it to mention open-artifact", out.String())
	}
}

func TestServeHelpListsFlags(t *testing.T) {
	t.Parallel()

	root := NewRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"serve", "--help"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("execute serve --help: %v", err)
	}
	help := out.String()
	for _, flag := range []string{
		"--bucket-url",
		"--bucket-prefix",
		"--port",
		"--log-level",
		"--repo-type",
		"--authn-oidc-issuers",
		"--pypi-max-upload-bytes",
		"--pypi-simple-index-cache-ttl",
		"--pypi-proxy-index-cache-ttl",
		"--pypi-proxy-negative-cache-ttl",
		"--npm-max-upload-bytes",
		"--npm-proxy-packument-memo-ttl",
		"--npm-proxy-packument-cache-ttl",
		"--npm-proxy-negative-cache-ttl",
		"--maven-max-upload-bytes",
	} {
		if !strings.Contains(help, flag) {
			t.Errorf("serve --help missing %q\n%s", flag, help)
		}
	}
}

func TestAdminServeHelpOmitsDataPlaneFlags(t *testing.T) {
	t.Parallel()

	root := NewRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"admin", "serve", "--help"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("execute admin serve --help: %v", err)
	}
	help := out.String()
	if !strings.Contains(help, "--bucket-url") {
		t.Errorf("admin serve --help missing --bucket-url\n%s", help)
	}
	if strings.Contains(help, "--repo-type") {
		t.Errorf("admin serve --help unexpectedly lists data-plane --repo-type\n%s", help)
	}
}
