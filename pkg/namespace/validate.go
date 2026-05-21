package namespace

import (
	"errors"
	"fmt"
	"net/url"
)

// Sentinel errors. Surfaces and the admin handler match them with errors.Is
// and map them to HTTP status codes.
var (
	// ErrInvalidName is returned (wrapped with context) when a namespace name
	// fails validation.
	ErrInvalidName = errors.New("namespace: invalid name")
	// ErrUnsupportedSchemaVersion is returned when a spec's schema_version is
	// newer than this binary understands.
	ErrUnsupportedSchemaVersion = errors.New("namespace: unsupported schema version")
	// ErrInvalidProxy is returned when a spec's mode/proxy combination is
	// invalid (unknown mode, missing/invalid upstream, or a proxy block on a
	// hosted namespace).
	ErrInvalidProxy = errors.New("namespace: invalid proxy")
	// ErrInvalidPolicy is returned when a spec's policy carries a malformed
	// subject matcher (empty matcher, unknown/reserved kind, empty claim key,
	// or a regex that does not compile).
	ErrInvalidPolicy = errors.New("namespace: invalid policy")
	// ErrNotFound is returned when a namespace does not exist.
	ErrNotFound = errors.New("namespace: not found")
	// ErrNotEmpty is returned when deleting a namespace that still holds
	// package data.
	ErrNotEmpty = errors.New("namespace: not empty")
)

// reservedNames are namespace names that collide with reserved URL roots,
// observability endpoints, format roots, or internal prefixes.
var reservedNames = map[string]bool{
	"admin":         true,
	"healthz":       true,
	"readyz":        true,
	"metrics":       true,
	"simple":        true,
	"maven2":        true,
	"v2":            true,
	"npm":           true,
	"pypi":          true,
	"_control":      true,
	"_proxy-cache":  true,
	"open-artifact": true,
}

// ValidateName checks a namespace name against the naming rules: 1-64 chars,
// lowercase ASCII letters/digits/'-' only, no leading/trailing '-', no leading
// '_' or '.', and not a reserved name. It returns ErrInvalidName wrapped with
// context on failure.
func ValidateName(name string) error {
	if n := len(name); n < 1 || n > 64 {
		return fmt.Errorf("%w: %q: want 1-64 characters", ErrInvalidName, name)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return fmt.Errorf("%w: %q: only lowercase letters, digits, and '-' are allowed", ErrInvalidName, name)
		}
	}
	if name[0] == '-' || name[len(name)-1] == '-' {
		return fmt.Errorf("%w: %q: must not start or end with '-'", ErrInvalidName, name)
	}
	// Leading '_'/'.' can never occur given the character class above, but the
	// reserved list still carries '_'-prefixed entries for defense in depth.
	if reservedNames[name] {
		return fmt.Errorf("%w: %q: reserved name", ErrInvalidName, name)
	}
	return nil
}

// normalizeForWrite validates a spec for storage and returns its normalized
// form: schema_version stamped to current, an explicit "hosted" mode collapsed
// to empty, and proxy/mode consistency enforced.
func normalizeForWrite(spec Spec) (Spec, error) {
	if spec.SchemaVersion > CurrentSchemaVersion {
		return Spec{}, fmt.Errorf("%w: spec declares version %d, this binary understands up to version %d",
			ErrUnsupportedSchemaVersion, spec.SchemaVersion, CurrentSchemaVersion)
	}
	spec.SchemaVersion = CurrentSchemaVersion

	switch spec.Mode {
	case ModeHosted:
		spec.Mode = "" // Keep hosted JSON compact.
		fallthrough
	case "":
		if !proxyEmpty(spec.Proxy) {
			return Spec{}, fmt.Errorf("%w: hosted namespace must not carry a proxy block", ErrInvalidProxy)
		}
	case ModeProxy:
		if err := validateUpstream(spec.Proxy.Upstream); err != nil {
			return Spec{}, err
		}
	default:
		return Spec{}, fmt.Errorf("%w: unknown mode %q", ErrInvalidProxy, spec.Mode)
	}

	if _, err := compilePolicy(spec.Policy); err != nil {
		return Spec{}, err
	}

	return spec, nil
}

// normalizeForRead applies read-time defaults: a missing schema_version means
// version 1. A stored version newer than this binary understands is rejected.
func normalizeForRead(spec Spec) (Spec, error) {
	if spec.SchemaVersion == 0 {
		spec.SchemaVersion = CurrentSchemaVersion
	}
	if spec.SchemaVersion > CurrentSchemaVersion {
		return Spec{}, fmt.Errorf("%w: stored spec is version %d, this binary understands up to version %d",
			ErrUnsupportedSchemaVersion, spec.SchemaVersion, CurrentSchemaVersion)
	}
	return spec, nil
}

// proxyEmpty reports whether a proxy block carries no configuration.
func proxyEmpty(p Proxy) bool {
	return p.Upstream == "" && len(p.Filters) == 0
}

// validateUpstream requires an absolute http(s) URL with a host.
func validateUpstream(raw string) error {
	if raw == "" {
		return fmt.Errorf("%w: proxy mode requires proxy.upstream", ErrInvalidProxy)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%w: proxy.upstream %q: %v", ErrInvalidProxy, raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%w: proxy.upstream %q: must be an http or https URL", ErrInvalidProxy, raw)
	}
	if u.Host == "" {
		return fmt.Errorf("%w: proxy.upstream %q: must include a host", ErrInvalidProxy, raw)
	}
	return nil
}
