package filter

import (
	"errors"
	"testing"
	"time"
)

func TestValidate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		specs   []Spec
		wantErr bool
	}{
		{name: "empty chain", specs: nil},
		{name: "allow with pattern", specs: []Spec{{Kind: KindAllow, Patterns: []string{"@myorg/*"}}}},
		{name: "allow with rule", specs: []Spec{{Kind: KindAllow, Rules: []Rule{{Package: "requests"}}}}},
		{name: "deny with rule pkg+ver", specs: []Spec{{Kind: KindDeny, Rules: []Rule{{Package: "log4j-core", Version: "2.14.*"}}}}},
		{name: "deny version-only rule", specs: []Spec{{Kind: KindDeny, Rules: []Rule{{Version: "1.0.0"}}}}},
		{name: "delay", specs: []Spec{{Kind: KindDelay, MinAge: "24h"}}},
		{name: "exact string pattern", specs: []Spec{{Kind: KindAllow, Patterns: []string{"requests"}}}},

		{name: "unknown kind", specs: []Spec{{Kind: "mirror"}}, wantErr: true},
		{name: "empty kind", specs: []Spec{{Kind: ""}}, wantErr: true},
		{name: "allow with neither pattern nor rule", specs: []Spec{{Kind: KindAllow}}, wantErr: true},
		{name: "deny with neither pattern nor rule", specs: []Spec{{Kind: KindDeny}}, wantErr: true},
		{name: "allow with min_age", specs: []Spec{{Kind: KindAllow, Patterns: []string{"*"}, MinAge: "1h"}}, wantErr: true},
		{name: "rule with neither field", specs: []Spec{{Kind: KindAllow, Rules: []Rule{{}}}}, wantErr: true},
		{name: "invalid glob pattern", specs: []Spec{{Kind: KindAllow, Patterns: []string{"[bad"}}}, wantErr: true},
		{name: "invalid glob in rule package", specs: []Spec{{Kind: KindDeny, Rules: []Rule{{Package: "[bad"}}}}, wantErr: true},
		{name: "invalid glob in rule version", specs: []Spec{{Kind: KindDeny, Rules: []Rule{{Version: "[bad"}}}}, wantErr: true},
		{name: "delay missing min_age", specs: []Spec{{Kind: KindDelay}}, wantErr: true},
		{name: "delay unparsable min_age", specs: []Spec{{Kind: KindDelay, MinAge: "soon"}}, wantErr: true},
		{name: "delay zero min_age", specs: []Spec{{Kind: KindDelay, MinAge: "0s"}}, wantErr: true},
		{name: "delay negative min_age", specs: []Spec{{Kind: KindDelay, MinAge: "-1h"}}, wantErr: true},
		{name: "delay with patterns", specs: []Spec{{Kind: KindDelay, MinAge: "1h", Patterns: []string{"*"}}}, wantErr: true},
		{name: "delay with rules", specs: []Spec{{Kind: KindDelay, MinAge: "1h", Rules: []Rule{{Package: "x"}}}}, wantErr: true},
		{name: "second filter invalid", specs: []Spec{{Kind: KindAllow, Patterns: []string{"*"}}, {Kind: "bogus"}}, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := Validate(tc.specs)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Validate() = nil, want error")
				}
				if !errors.Is(err, ErrInvalidFilter) {
					t.Fatalf("Validate() error = %v, want errors.Is ErrInvalidFilter", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}

func ptr(t time.Time) *time.Time { return &t }

func TestChainDecide(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC)
	old := now.Add(-48 * time.Hour)
	recent := now.Add(-1 * time.Hour)

	cases := []struct {
		name  string
		specs []Spec
		ref   Ref
		want  Outcome
	}{
		{
			name:  "empty chain defaults to allow",
			specs: nil,
			ref:   Ref{Package: "requests", Version: "1.0.0"},
			want:  OutcomeAllow,
		},
		{
			name:  "allow pattern matches package",
			specs: []Spec{{Kind: KindAllow, Patterns: []string{"@myorg/*"}}, {Kind: KindDeny, Patterns: []string{"*"}}},
			ref:   Ref{Package: "@myorg/utils", Version: "1.0.0"},
			want:  OutcomeAllow,
		},
		{
			name:  "deny catches what allow abstains on",
			specs: []Spec{{Kind: KindAllow, Patterns: []string{"@myorg/*"}}, {Kind: KindDeny, Patterns: []string{"*"}}},
			ref:   Ref{Package: "evil", Version: "1.0.0"},
			want:  OutcomeDeny,
		},
		{
			name:  "first decision wins: deny before allow",
			specs: []Spec{{Kind: KindDeny, Patterns: []string{"log4j-*"}}, {Kind: KindAllow, Patterns: []string{"*"}}},
			ref:   Ref{Package: "log4j-core", Version: "2.14.1"},
			want:  OutcomeDeny,
		},
		{
			name:  "star does not cross slash for scoped names",
			specs: []Spec{{Kind: KindDeny, Patterns: []string{"@*"}}},
			ref:   Ref{Package: "@scope/name", Version: "1.0.0"},
			want:  OutcomeAllow, // "@*" must not match "@scope/name"; chain abstains -> default allow
		},
		{
			name:  "scoped glob with slash matches",
			specs: []Spec{{Kind: KindDeny, Patterns: []string{"@scope/*"}}},
			ref:   Ref{Package: "@scope/name", Version: "1.0.0"},
			want:  OutcomeDeny,
		},
		{
			name:  "rule package+version both required",
			specs: []Spec{{Kind: KindDeny, Rules: []Rule{{Package: "log4j-core", Version: "2.14.*"}}}},
			ref:   Ref{Package: "log4j-core", Version: "2.14.1"},
			want:  OutcomeDeny,
		},
		{
			name:  "rule package+version no version match abstains",
			specs: []Spec{{Kind: KindDeny, Rules: []Rule{{Package: "log4j-core", Version: "2.14.*"}}}},
			ref:   Ref{Package: "log4j-core", Version: "2.17.1"},
			want:  OutcomeAllow,
		},
		{
			name:  "package-only rule matches any version",
			specs: []Spec{{Kind: KindDeny, Rules: []Rule{{Package: "leftpad"}}}},
			ref:   Ref{Package: "leftpad", Version: "9.9.9"},
			want:  OutcomeDeny,
		},
		{
			name:  "version-only rule matches across packages",
			specs: []Spec{{Kind: KindDeny, Rules: []Rule{{Version: "0.0.0"}}}},
			ref:   Ref{Package: "anything", Version: "0.0.0"},
			want:  OutcomeDeny,
		},
		{
			name:  "delay allows old enough",
			specs: []Spec{{Kind: KindDelay, MinAge: "24h"}},
			ref:   Ref{Package: "x", Version: "1.0.0", PublishedAt: ptr(old)},
			want:  OutcomeAllow,
		},
		{
			name:  "delay denies too new",
			specs: []Spec{{Kind: KindDelay, MinAge: "24h"}},
			ref:   Ref{Package: "x", Version: "1.0.0", PublishedAt: ptr(recent)},
			want:  OutcomeDeny,
		},
		{
			name:  "delay needs metadata when publish time unknown",
			specs: []Spec{{Kind: KindDelay, MinAge: "24h"}},
			ref:   Ref{Package: "x", Version: "1.0.0"},
			want:  OutcomeNeedsMetadata,
		},
		{
			name:  "delay age exactly min_age allows",
			specs: []Spec{{Kind: KindDelay, MinAge: "24h"}},
			ref:   Ref{Package: "x", Version: "1.0.0", PublishedAt: ptr(now.Add(-24 * time.Hour))},
			want:  OutcomeAllow,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			chain, err := Compile(tc.specs)
			if err != nil {
				t.Fatalf("Compile() = %v", err)
			}
			got := chain.DecideAt(tc.ref, now)
			if got.Outcome != tc.want {
				t.Fatalf("DecideAt() outcome = %v (%s), want %v", got.Outcome, got.Reason, tc.want)
			}
			if got.Reason == "" {
				t.Fatalf("DecideAt() reason is empty; decisions must carry reason info")
			}
		})
	}
}

func TestDecisionHelpers(t *testing.T) {
	t.Parallel()
	if !(Decision{Outcome: OutcomeAllow}).Allowed() {
		t.Fatal("OutcomeAllow.Allowed() = false")
	}
	if !(Decision{Outcome: OutcomeDeny}).Denied() {
		t.Fatal("OutcomeDeny.Denied() = false")
	}
	if !(Decision{Outcome: OutcomeNeedsMetadata}).NeedsMetadata() {
		t.Fatal("OutcomeNeedsMetadata.NeedsMetadata() = false")
	}
}
