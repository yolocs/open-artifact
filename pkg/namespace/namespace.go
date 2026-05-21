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

// Proxy configures a proxy-mode namespace's upstream and filter chain. Filter
// validation details are owned by the proxy work (#17).
type Proxy struct {
	Upstream string       `json:"upstream,omitempty"`
	Filters  []FilterSpec `json:"filters,omitempty"`
}

// FilterSpec is one entry in a proxy namespace's ordered allow/deny/delay
// filter chain. Its validation semantics are defined by #17; here it only
// needs to round-trip.
type FilterSpec struct {
	Action  string `json:"action,omitempty"`
	Pattern string `json:"pattern,omitempty"`
}

// Policy lists the subject matchers that may read or write the namespace. An
// empty policy is deny-all; matcher validation and enforcement are owned by
// #7.
type Policy struct {
	Readers []SubjectMatcher `json:"readers,omitempty"`
	Writers []SubjectMatcher `json:"writers,omitempty"`
}

// SubjectMatcher matches an authenticated subject against a namespace policy.
// The match semantics are owned by #7; here it only needs to round-trip.
type SubjectMatcher struct {
	Issuer  string `json:"issuer,omitempty"`
	Subject string `json:"subject,omitempty"`
}

// IsProxy reports whether the spec selects proxy mode.
func (s Spec) IsProxy() bool { return s.Mode == ModeProxy }

// IsHosted reports whether the spec selects hosted mode (the default).
func (s Spec) IsHosted() bool { return s.Mode == "" || s.Mode == ModeHosted }
