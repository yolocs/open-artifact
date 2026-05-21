package namespace

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/yolocs/open-artifact/pkg/auth"
)

// compiledPolicy is a Policy with its regexes compiled and its matchers
// normalized, ready to authorize subjects. It implements auth.Authorizer.
type compiledPolicy struct {
	readers []compiledMatcher
	writers []compiledMatcher
}

// compiledMatcher is one normalized SubjectMatcher. A nil regex means the
// corresponding field is unconstrained; an empty equality field is likewise
// unconstrained. kind is always populated (defaulted to KindOIDC).
type compiledMatcher struct {
	issuer string
	sub    *regexp.Regexp
	email  string
	claims []compiledClaim
	kind   string
}

type compiledClaim struct {
	key string
	re  *regexp.Regexp
}

// compilePolicy validates and compiles a Policy. It returns ErrInvalidPolicy
// (wrapped) for an empty matcher, an unknown/reserved kind, an empty claim key,
// or a regex that does not compile.
func compilePolicy(p Policy) (*compiledPolicy, error) {
	readers, err := compileMatchers(p.Readers, "readers")
	if err != nil {
		return nil, err
	}
	writers, err := compileMatchers(p.Writers, "writers")
	if err != nil {
		return nil, err
	}
	return &compiledPolicy{readers: readers, writers: writers}, nil
}

func compileMatchers(ms []SubjectMatcher, role string) ([]compiledMatcher, error) {
	out := make([]compiledMatcher, 0, len(ms))
	for i, m := range ms {
		cm, err := compileMatcher(m)
		if err != nil {
			return nil, fmt.Errorf("%w: %s[%d]: %v", ErrInvalidPolicy, role, i, err)
		}
		out = append(out, cm)
	}
	return out, nil
}

func compileMatcher(m SubjectMatcher) (compiledMatcher, error) {
	if m.Issuer == "" && m.SubMatch == "" && m.Email == "" && len(m.ClaimsMatch) == 0 && m.Kind == "" {
		return compiledMatcher{}, fmt.Errorf("matcher must populate at least one field")
	}

	kind := m.Kind
	switch kind {
	case "":
		kind = KindOIDC
	case KindOIDC:
	case KindBasicToken:
		return compiledMatcher{}, fmt.Errorf("kind %q is reserved and not supported in v1", KindBasicToken)
	default:
		return compiledMatcher{}, fmt.Errorf("unknown kind %q", kind)
	}

	cm := compiledMatcher{issuer: m.Issuer, email: m.Email, kind: kind}
	if m.SubMatch != "" {
		re, err := compileAnchored(m.SubMatch)
		if err != nil {
			return compiledMatcher{}, fmt.Errorf("sub_match: %v", err)
		}
		cm.sub = re
	}
	for key, pat := range m.ClaimsMatch {
		if key == "" {
			return compiledMatcher{}, fmt.Errorf("claims_match: empty claim key")
		}
		re, err := compileAnchored(pat)
		if err != nil {
			return compiledMatcher{}, fmt.Errorf("claims_match[%q]: %v", key, err)
		}
		cm.claims = append(cm.claims, compiledClaim{key: key, re: re})
	}
	return cm, nil
}

// compileAnchored compiles an RE2 pattern anchored at both ends. An end already
// carrying its anchor is left as-is so callers may anchor explicitly.
func compileAnchored(pat string) (*regexp.Regexp, error) {
	anchored := pat
	if !strings.HasPrefix(anchored, "^") {
		anchored = "^" + anchored
	}
	if !strings.HasSuffix(anchored, "$") {
		anchored = anchored + "$"
	}
	return regexp.Compile(anchored)
}

// Authorize reports whether ac may perform op. A nil AuthContext is always
// unauthorized; an unrecognized op returns ErrUnknownOp. OpRead consults only
// readers and OpWrite only writers; any matcher in the selected list may allow.
func (p *compiledPolicy) Authorize(_ context.Context, ac *auth.AuthContext, op auth.Op) error {
	if ac == nil {
		return auth.ErrUnauthorized
	}
	var matchers []compiledMatcher
	switch op {
	case auth.OpRead:
		matchers = p.readers
	case auth.OpWrite:
		matchers = p.writers
	default:
		return fmt.Errorf("%w: %q", auth.ErrUnknownOp, op)
	}
	for _, m := range matchers {
		if m.matches(ac) {
			return nil
		}
	}
	return auth.ErrUnauthorized
}

// matches reports whether every populated field of m matches ac.
func (m compiledMatcher) matches(ac *auth.AuthContext) bool {
	if ac.Kind != m.kind {
		return false
	}
	if m.issuer != "" && ac.Issuer != m.issuer {
		return false
	}
	if m.sub != nil && !m.sub.MatchString(ac.ID) {
		return false
	}
	if m.email != "" && ac.Email != m.email {
		return false
	}
	for _, c := range m.claims {
		v, ok := ac.Claims[c.key]
		if !ok {
			return false
		}
		s, ok := claimString(v)
		if !ok || !c.re.MatchString(s) {
			return false
		}
	}
	return true
}

// claimString renders a claim value for regex matching: string values are
// compared directly; anything else is JSON-encoded, which orders object keys
// stably so matching is deterministic.
func claimString(v any) (string, bool) {
	if s, ok := v.(string); ok {
		return s, true
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", false
	}
	return string(b), true
}
