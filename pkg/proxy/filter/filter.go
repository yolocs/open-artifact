// Package filter is the proxy allow/deny/delay filter chain: the persisted
// config schema, its validation, and the runtime decision engine.
//
// The schema lives here (not in pkg/namespace) so there is one source of truth
// for the filter shape, its validation, and the matching semantics. The
// namespace control plane embeds filter.Spec in its proxy block and validates
// the chain through Validate; proxy-mode format surfaces compile the same specs
// into a Chain and ask it to Decide each artifact download.
//
// The package is a pure leaf: it imports only the standard library and has no
// knowledge of HTTP, blobs, namespaces, or any package format.
package filter

import (
	"errors"
	"fmt"
	"path"
	"time"
)

// Filter kinds.
const (
	// KindAllow allows a ref if any of its patterns/rules match, then stops the
	// chain; otherwise it abstains.
	KindAllow = "allow"
	// KindDeny denies a ref if any of its patterns/rules match, then stops the
	// chain; otherwise it abstains.
	KindDeny = "deny"
	// KindDelay quarantines freshly published artifacts: a ref younger than
	// MinAge is denied, one at least MinAge old is allowed, and one whose
	// publish time is unknown needs metadata before it can be decided.
	KindDelay = "delay"
)

// ErrInvalidFilter is returned (wrapped with context) when a filter spec fails
// validation. Callers match it with errors.Is.
var ErrInvalidFilter = errors.New("filter: invalid")

// Spec is one entry in a proxy namespace's ordered filter chain. It is the
// persisted JSON shape embedded in the namespace proxy block.
//
// An allow/deny filter carries patterns and/or rules and must not set min_age;
// a delay filter carries min_age and must not set patterns or rules.
type Spec struct {
	Kind     string   `json:"kind"`
	Patterns []string `json:"patterns,omitempty"`
	Rules    []Rule   `json:"rules,omitempty"`
	MinAge   string   `json:"min_age,omitempty"`
}

// Rule matches an artifact by package and/or version glob. A populated field
// constrains the match; both populated fields are ANDed. A rule with only a
// package matches any version; a rule with only a version matches that version
// across any package. A rule with neither is invalid.
type Rule struct {
	Package string `json:"package,omitempty"`
	Version string `json:"version,omitempty"`
}

// Ref identifies the artifact a decision is about. PublishedAt is nil when the
// upstream publish time is not yet known; a delay filter then asks for
// metadata rather than guessing.
type Ref struct {
	Package     string
	Version     string
	PublishedAt *time.Time
}

// Outcome is the result of evaluating a chain against a Ref.
type Outcome int

const (
	// OutcomeAllow permits the download.
	OutcomeAllow Outcome = iota
	// OutcomeDeny refuses the download.
	OutcomeDeny
	// OutcomeNeedsMetadata means a delay filter could not decide because the
	// ref's publish time is unknown; the caller should resolve it and re-decide.
	OutcomeNeedsMetadata
)

// Decision is a chain evaluation result. Reason carries enough context for logs
// and metrics; it is human-readable and not part of any wire contract.
type Decision struct {
	Outcome Outcome
	Reason  string
}

// Allowed reports whether the decision permits the download.
func (d Decision) Allowed() bool { return d.Outcome == OutcomeAllow }

// Denied reports whether the decision refuses the download.
func (d Decision) Denied() bool { return d.Outcome == OutcomeDeny }

// NeedsMetadata reports whether the decision is blocked on an unknown publish
// time.
func (d Decision) NeedsMetadata() bool { return d.Outcome == OutcomeNeedsMetadata }

// Chain is a compiled, validated filter chain ready to make decisions. The zero
// value (and a chain compiled from no specs) allows everything.
type Chain struct {
	filters []compiledFilter
}

// compiledFilter is a validated Spec: patterns and rule globs have been checked
// with path.Match, and a delay's MinAge has been parsed to a positive duration.
type compiledFilter struct {
	kind     string
	patterns []string
	rules    []Rule
	minAge   time.Duration
}

// Compile validates the specs and returns a decision-ready Chain. It returns
// ErrInvalidFilter (wrapped with the offending index) on the first invalid
// spec.
func Compile(specs []Spec) (*Chain, error) {
	out := make([]compiledFilter, 0, len(specs))
	for i, s := range specs {
		cf, err := compileFilter(s)
		if err != nil {
			return nil, fmt.Errorf("%w: filter[%d]: %v", ErrInvalidFilter, i, err)
		}
		out = append(out, cf)
	}
	return &Chain{filters: out}, nil
}

// Validate reports whether specs form a valid chain. It is Compile with the
// chain discarded — used by namespace spec validation.
func Validate(specs []Spec) error {
	_, err := Compile(specs)
	return err
}

func compileFilter(s Spec) (compiledFilter, error) {
	switch s.Kind {
	case KindAllow, KindDeny:
		if len(s.Patterns) == 0 && len(s.Rules) == 0 {
			return compiledFilter{}, fmt.Errorf("%q requires at least one pattern or rule", s.Kind)
		}
		if s.MinAge != "" {
			return compiledFilter{}, fmt.Errorf("%q must not set min_age", s.Kind)
		}
		for _, p := range s.Patterns {
			if err := validateGlob(p); err != nil {
				return compiledFilter{}, fmt.Errorf("pattern %q: %v", p, err)
			}
		}
		for j, r := range s.Rules {
			if r.Package == "" && r.Version == "" {
				return compiledFilter{}, fmt.Errorf("rule[%d]: must set package or version", j)
			}
			if err := validateGlob(r.Package); err != nil {
				return compiledFilter{}, fmt.Errorf("rule[%d] package %q: %v", j, r.Package, err)
			}
			if err := validateGlob(r.Version); err != nil {
				return compiledFilter{}, fmt.Errorf("rule[%d] version %q: %v", j, r.Version, err)
			}
		}
		return compiledFilter{kind: s.Kind, patterns: s.Patterns, rules: s.Rules}, nil

	case KindDelay:
		if len(s.Patterns) > 0 || len(s.Rules) > 0 {
			return compiledFilter{}, fmt.Errorf("delay must not set patterns or rules")
		}
		if s.MinAge == "" {
			return compiledFilter{}, fmt.Errorf("delay requires min_age")
		}
		d, err := time.ParseDuration(s.MinAge)
		if err != nil {
			return compiledFilter{}, fmt.Errorf("min_age %q: %v", s.MinAge, err)
		}
		if d <= 0 {
			return compiledFilter{}, fmt.Errorf("min_age must be positive, got %s", d)
		}
		return compiledFilter{kind: KindDelay, minAge: d}, nil

	default:
		return compiledFilter{}, fmt.Errorf("unknown kind %q", s.Kind)
	}
}

// validateGlob reports whether p is a legal path.Match pattern. An empty
// pattern is legal (it is an unconstrained field on a Rule). path.Match never
// matches anything on the empty subject except an empty pattern, so probing
// with "" only surfaces ErrBadPattern.
func validateGlob(p string) error {
	if p == "" {
		return nil
	}
	if _, err := path.Match(p, ""); err != nil {
		return err
	}
	return nil
}

// Decide evaluates the chain against ref using the current time. Filters only
// apply to artifact/file downloads; index/metadata listing requests must not be
// run through a chain.
func (c *Chain) Decide(ref Ref) Decision {
	return c.DecideAt(ref, time.Now())
}

// DecideAt evaluates the chain against ref at a fixed time. The first
// allow/deny decision wins; an abstaining filter moves to the next; if every
// filter abstains the default is allow.
func (c *Chain) DecideAt(ref Ref, now time.Time) Decision {
	for i, f := range c.filters {
		switch f.kind {
		case KindAllow:
			if f.matches(ref) {
				return Decision{OutcomeAllow, fmt.Sprintf("allow: filter[%d] matched %q", i, ref.Package)}
			}
		case KindDeny:
			if f.matches(ref) {
				return Decision{OutcomeDeny, fmt.Sprintf("deny: filter[%d] matched %q", i, ref.Package)}
			}
		case KindDelay:
			if ref.PublishedAt == nil {
				return Decision{OutcomeNeedsMetadata, fmt.Sprintf("delay: filter[%d] needs publish time", i)}
			}
			age := now.Sub(*ref.PublishedAt)
			if age >= f.minAge {
				return Decision{OutcomeAllow, fmt.Sprintf("delay: filter[%d] age %s >= %s", i, age, f.minAge)}
			}
			return Decision{OutcomeDeny, fmt.Sprintf("delay: filter[%d] age %s < %s", i, age, f.minAge)}
		}
	}
	return Decision{OutcomeAllow, "default allow: no filter matched"}
}

// matches reports whether ref matches any pattern or rule of an allow/deny
// filter. Patterns match the package name; rules match package and/or version.
func (f compiledFilter) matches(ref Ref) bool {
	for _, p := range f.patterns {
		if ok, _ := path.Match(p, ref.Package); ok {
			return true
		}
	}
	for _, r := range f.rules {
		if matchRule(r, ref) {
			return true
		}
	}
	return false
}

// matchRule reports whether ref satisfies a rule. An empty field is
// unconstrained; populated fields are ANDed. Globs use path.Match semantics, so
// '*' does not cross '/' — which matters for npm scoped names like @scope/name.
func matchRule(r Rule, ref Ref) bool {
	if r.Package != "" {
		if ok, _ := path.Match(r.Package, ref.Package); !ok {
			return false
		}
	}
	if r.Version != "" {
		if ok, _ := path.Match(r.Version, ref.Version); !ok {
			return false
		}
	}
	return true
}
