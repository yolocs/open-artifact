// Package namespace is the namespace control plane and the data-plane
// namespace factory. A namespace is the canonical top-level partition of the
// bucket: everything that belongs to it lives under open-artifact/v1/<ns>/,
// and its own mode/policy/proxy metadata is the <ns>/.meta object.
//
// The control-plane Store owns the <ns>/.meta object and is its only writer.
// The data-plane Registry yields namespace-and-format-scoped core.Store
// handles (bound to scope <ns>/<fmt>) plus the namespace Spec for
// hosted/proxy dispatch. Listing is the index: a namespace exists once its
// .meta is written, and emptiness is "no package data under <ns>/".
package namespace

import "github.com/yolocs/open-artifact/pkg/proxy/filter"

// CurrentSchemaVersion is the namespace spec schema version this binary
// understands. A stored spec with a higher version is rejected.
const CurrentSchemaVersion = 1

// Namespace modes. The empty string is equivalent to ModeHosted and is the
// stored form for hosted namespaces (kept compact).
const (
	ModeHosted = "hosted"
	ModeProxy  = "proxy"
)

// Namespace is a control-plane object: a name plus its spec.
type Namespace struct {
	Name string `json:"name"`
	Spec Spec   `json:"spec"`
}

// Spec carries a namespace's mode, policy, proxy configuration, and an opaque
// per-format knob map. Unknown JSON in Format must round-trip unchanged.
type Spec struct {
	SchemaVersion int            `json:"schema_version,omitempty"`
	Mode          string         `json:"mode,omitempty"`
	Policy        Policy         `json:"policy,omitempty"`
	Proxy         Proxy          `json:"proxy,omitempty"`
	Format        map[string]any `json:"format,omitempty"`
}

// Proxy configures a proxy-mode namespace's upstream and ordered
// allow/deny/delay filter chain. The filter schema, validation, and decision
// semantics live in pkg/proxy/filter; the chain is validated as part of spec
// validation and applies only to artifact downloads.
type Proxy struct {
	Upstream string        `json:"upstream,omitempty"`
	Filters  []filter.Spec `json:"filters,omitempty"`
}

// Subject-matcher kinds. The empty kind is equivalent to KindOIDC. KindBasicToken
// is reserved for a future static-token credential and is rejected in v1.
const (
	KindOIDC       = "oidc"
	KindAnonymous  = "anonymous"
	KindBasicToken = "basictoken"
)

// Policy lists the subject matchers that may read or write the namespace.
// Readers and Writers are independent — write does not imply read — and an
// empty policy is deny-all.
type Policy struct {
	Readers []SubjectMatcher `json:"readers,omitempty"`
	Writers []SubjectMatcher `json:"writers,omitempty"`
}

// SubjectMatcher matches an authenticated subject against a namespace policy.
// A populated field constrains the match; all populated fields within a matcher
// are ANDed. Issuer and Email compare for equality; SubMatch and every
// ClaimsMatch value are RE2 regexes anchored at both ends. An empty Kind means
// KindOIDC.
type SubjectMatcher struct {
	Issuer      string            `json:"issuer,omitempty"`
	SubMatch    string            `json:"sub_match,omitempty"`
	Email       string            `json:"email,omitempty"`
	ClaimsMatch map[string]string `json:"claims_match,omitempty"`
	Kind        string            `json:"kind,omitempty"`
}

// IsProxy reports whether the spec selects proxy mode.
func (s Spec) IsProxy() bool { return s.Mode == ModeProxy }

// IsHosted reports whether the spec selects hosted mode (the default).
func (s Spec) IsHosted() bool { return s.Mode == "" || s.Mode == ModeHosted }
