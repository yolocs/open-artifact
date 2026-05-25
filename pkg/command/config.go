package command

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"

	"github.com/yolocs/open-artifact/pkg/surface/npm"
	"github.com/yolocs/open-artifact/pkg/surface/pypi"
)

// envPrefix is the env-var prefix for every runtime flag: a flag --foo-bar is
// also settable via OPEN_ARTIFACT_FOO_BAR.
const envPrefix = "OPEN_ARTIFACT"

// defaultDataPort and defaultAdminPort are the listen ports for `serve` and
// `admin serve` respectively.
const (
	defaultDataPort  = 8080
	defaultAdminPort = 8081
)

// reservedObservabilityPaths are the fixed liveness/readiness endpoints the
// observability wrapper registers; --metrics-path may not reuse one or the
// shared mux would panic on a duplicate pattern at startup.
var reservedObservabilityPaths = map[string]bool{"/healthz": true, "/readyz": true}

// repoTypes is the eventual allow-list of data-plane formats. Real serving is
// out of scope here (#19-#25); this issue only validates the flag value. The
// internal "echo" type is reserved for OIDC CI wiring (#25).
var repoTypes = map[string]bool{"pypi": true, "npm": true, "maven": true, "echo": true}

// runtimeConfig is the resolved, validated runtime configuration shared by the
// data and admin planes. Data-plane-only fields are populated for `serve` and
// left zero for `admin serve`.
type runtimeConfig struct {
	Port          int
	BucketURL     string
	BucketPrefix  string
	EnableMetrics bool
	MetricsPath   string
	LogLevel      string
	LogFormat     string
	LogDebug      bool

	// Data-plane only. The authenticator is built from these by the serve
	// wiring; format routes (#25) install the middleware.
	RepoType          string
	DisableAuthn      bool
	AuthnKind         string
	AuthnOIDCIssuers  []string
	AuthnOIDCAudience string
	PyPI              pypi.Config
	NPM               npm.Config

	// authnKindSet records whether --authn-kind was set explicitly (by flag or
	// env) rather than left at its default, so it can be flagged as mutually
	// exclusive with --disable-authn.
	authnKindSet bool
}

// addSharedFlags registers the flags present on both planes. defaultPort
// differs per plane.
func addSharedFlags(f *pflag.FlagSet, defaultPort int) {
	f.Int("port", defaultPort, "HTTP listen port (also read from PORT)")
	f.String("bucket-url", "", "gocloud.dev/blob bucket URL, e.g. mem://, file:///data, s3://bucket (required)")
	f.String("bucket-prefix", "", "optional clean relative path prefix scoping this deployment within a shared bucket")
	f.Bool("enable-metrics", true, "expose Prometheus metrics")
	f.String("metrics-path", "/metrics", "path served for metrics")
	f.String("log-level", "info", "log level: debug, info, warn, error")
	f.String("log-format", "text", "log format: text, json")
	f.Bool("log-debug", false, "include caller/source details in logs")
}

// addDataPlaneFlags registers the flags present only on `serve`.
func addDataPlaneFlags(f *pflag.FlagSet) {
	f.String("repo-type", "", "repository format: pypi, npm, maven")
	f.Bool("disable-authn", false, "disable authentication (logs a warning)")
	f.String("authn-kind", "oidc", "authenticator kind: oidc")
	f.StringSlice("authn-oidc-issuers", nil, "comma-separated OIDC issuer URLs")
	f.String("authn-oidc-audience", "", "expected OIDC token audience")
	f.Int64("pypi-max-upload-bytes", pypi.DefaultMaxUploadBytes, "maximum PyPI multipart upload size in bytes; 0 uses the default cap")
	f.Duration("pypi-simple-index-cache-ttl", 60*time.Second, "per-process PyPI project simple-index cache TTL; 0 disables caching")
	f.Duration("pypi-proxy-index-cache-ttl", pypi.DefaultProxyIndexCacheTTL, "in-process PyPI proxy upstream-index cache TTL (burst absorber); 0 uses the default, negative disables caching")
	f.Duration("pypi-proxy-negative-cache-ttl", pypi.DefaultProxyNegativeCacheTTL, "how long an upstream PyPI 404 is remembered in proxy mode; 0 uses the default")
	f.Int64("npm-max-upload-bytes", npm.DefaultMaxUploadBytes, "maximum npm publish body size in bytes; 0 uses the default cap")
}

// newViper builds a viper bound to cmd's flags with OPEN_ARTIFACT env
// resolution. Precedence is flag > env > default. The platform PORT variable is
// bound to --port in addition to OPEN_ARTIFACT_PORT.
func newViper(cmd *cobra.Command) (*viper.Viper, error) {
	v := viper.New()
	v.SetEnvPrefix(envPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	v.AutomaticEnv()
	if err := v.BindEnv("port", envPrefix+"_PORT", "PORT"); err != nil {
		return nil, err
	}
	if err := v.BindPFlags(cmd.Flags()); err != nil {
		return nil, err
	}
	return v, nil
}

// resolveConfig reads, normalizes, and validates the configuration for a
// command. dataPlane selects whether data-plane-only flags are consulted.
func resolveConfig(cmd *cobra.Command, dataPlane bool) (*runtimeConfig, error) {
	v, err := newViper(cmd)
	if err != nil {
		return nil, err
	}

	cfg := &runtimeConfig{
		Port:          v.GetInt("port"),
		BucketURL:     strings.TrimSpace(v.GetString("bucket-url")),
		BucketPrefix:  v.GetString("bucket-prefix"),
		EnableMetrics: v.GetBool("enable-metrics"),
		MetricsPath:   v.GetString("metrics-path"),
		LogLevel:      v.GetString("log-level"),
		LogFormat:     v.GetString("log-format"),
		LogDebug:      v.GetBool("log-debug"),
	}
	if dataPlane {
		cfg.RepoType = strings.TrimSpace(v.GetString("repo-type"))
		cfg.DisableAuthn = v.GetBool("disable-authn")
		cfg.AuthnKind = strings.TrimSpace(v.GetString("authn-kind"))
		cfg.AuthnOIDCIssuers = splitCSV(v.GetStringSlice("authn-oidc-issuers"))
		cfg.AuthnOIDCAudience = strings.TrimSpace(v.GetString("authn-oidc-audience"))
		cfg.PyPI.MaxUploadBytes = v.GetInt64("pypi-max-upload-bytes")
		cfg.PyPI.SimpleIndexCacheTTL = v.GetDuration("pypi-simple-index-cache-ttl")
		cfg.PyPI.ProxyIndexCacheTTL = v.GetDuration("pypi-proxy-index-cache-ttl")
		cfg.PyPI.ProxyNegativeCacheTTL = v.GetDuration("pypi-proxy-negative-cache-ttl")
		cfg.NPM.MaxUploadBytes = v.GetInt64("npm-max-upload-bytes")
		_, authnKindEnv := os.LookupEnv(envPrefix + "_AUTHN_KIND")
		cfg.authnKindSet = cmd.Flags().Changed("authn-kind") || authnKindEnv
	}

	if err := cfg.validate(dataPlane); err != nil {
		return nil, err
	}
	return cfg, nil
}

// validate checks the resolved configuration, joining every problem into one
// error so a single run surfaces all of them.
func (c *runtimeConfig) validate(dataPlane bool) error {
	var errs []error

	if c.Port < 1 || c.Port > 65535 {
		errs = append(errs, fmt.Errorf("invalid --port %d: want 1-65535", c.Port))
	}
	if c.BucketURL == "" {
		errs = append(errs, errors.New("missing --bucket-url (or OPEN_ARTIFACT_BUCKET_URL)"))
	}
	if err := validateBucketPrefix(c.BucketPrefix); err != nil {
		errs = append(errs, err)
	}
	if !validLogLevel(c.LogLevel) {
		errs = append(errs, fmt.Errorf("invalid --log-level %q: want debug, info, warn, or error", c.LogLevel))
	}
	if !validLogFormat(c.LogFormat) {
		errs = append(errs, fmt.Errorf("invalid --log-format %q: want text or json", c.LogFormat))
	}
	if !strings.HasPrefix(c.MetricsPath, "/") {
		errs = append(errs, fmt.Errorf("invalid --metrics-path %q: must start with /", c.MetricsPath))
	}
	if reservedObservabilityPaths[c.MetricsPath] {
		errs = append(errs, fmt.Errorf("invalid --metrics-path %q: collides with a reserved observability endpoint", c.MetricsPath))
	}

	if dataPlane {
		if c.RepoType != "" && !repoTypes[c.RepoType] {
			errs = append(errs, fmt.Errorf("unsupported --repo-type %q: want pypi, npm, or maven", c.RepoType))
		}
		if c.PyPI.MaxUploadBytes < 0 {
			errs = append(errs, fmt.Errorf("invalid --pypi-max-upload-bytes %d: must be >= 0", c.PyPI.MaxUploadBytes))
		}
		if c.PyPI.SimpleIndexCacheTTL < 0 {
			errs = append(errs, fmt.Errorf("invalid --pypi-simple-index-cache-ttl %s: must be >= 0", c.PyPI.SimpleIndexCacheTTL))
		}
		if c.PyPI.ProxyNegativeCacheTTL < 0 {
			errs = append(errs, fmt.Errorf("invalid --pypi-proxy-negative-cache-ttl %s: must be >= 0", c.PyPI.ProxyNegativeCacheTTL))
		}
		if c.NPM.MaxUploadBytes < 0 {
			errs = append(errs, fmt.Errorf("invalid --npm-max-upload-bytes %d: must be >= 0", c.NPM.MaxUploadBytes))
		}
		errs = append(errs, c.validateAuthn()...)
	}

	return errors.Join(errs...)
}

// validateAuthn enforces that the data plane starts only with a usable
// authentication configuration: either authn is explicitly disabled, or the
// OIDC authenticator has both issuers and an audience. --disable-authn and an
// explicit --authn-kind are mutually exclusive.
func (c *runtimeConfig) validateAuthn() []error {
	var errs []error
	if c.DisableAuthn {
		if c.authnKindSet {
			errs = append(errs, errors.New("--disable-authn and --authn-kind are mutually exclusive"))
		}
		return errs
	}
	switch c.AuthnKind {
	case "oidc":
		if len(c.AuthnOIDCIssuers) == 0 {
			errs = append(errs, errors.New("--authn-kind=oidc requires --authn-oidc-issuers (or OPEN_ARTIFACT_AUTHN_OIDC_ISSUERS)"))
		}
		if c.AuthnOIDCAudience == "" {
			errs = append(errs, errors.New("--authn-kind=oidc requires --authn-oidc-audience (or OPEN_ARTIFACT_AUTHN_OIDC_AUDIENCE)"))
		}
	case "":
		errs = append(errs, errors.New("authentication is required: set --disable-authn or --authn-kind=oidc"))
	default:
		errs = append(errs, fmt.Errorf("unsupported --authn-kind %q: want oidc", c.AuthnKind))
	}
	return errs
}

// validateBucketPrefix accepts an empty prefix or a clean relative,
// slash-separated prefix. It rejects absolute paths, "..", empty segments, and
// segments beginning with "." so prefixes never collide with Store-owned
// dot-objects or escape the deployment subtree.
func validateBucketPrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	if strings.HasPrefix(prefix, "/") {
		return fmt.Errorf("invalid --bucket-prefix %q: must be relative, not absolute", prefix)
	}
	for _, seg := range strings.Split(prefix, "/") {
		switch {
		case seg == "":
			return fmt.Errorf("invalid --bucket-prefix %q: empty path segment", prefix)
		case seg == "..":
			return fmt.Errorf("invalid --bucket-prefix %q: %q is not allowed", prefix, "..")
		case strings.HasPrefix(seg, "."):
			return fmt.Errorf("invalid --bucket-prefix %q: segment %q may not begin with '.'", prefix, seg)
		}
	}
	return nil
}

func validLogLevel(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug", "info", "warn", "error":
		return true
	}
	return false
}

func validLogFormat(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "text", "json":
		return true
	}
	return false
}

// splitCSV normalizes a string slice that may contain comma-joined elements
// (the shape viper yields for a comma-separated env var) into individual,
// trimmed, non-empty values.
func splitCSV(in []string) []string {
	var out []string
	for _, item := range in {
		for _, part := range strings.Split(item, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}
