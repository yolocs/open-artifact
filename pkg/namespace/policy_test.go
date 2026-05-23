package namespace

import (
	"errors"
	"testing"

	"github.com/yolocs/open-artifact/pkg/auth"
)

func TestCompilePolicyValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		policy  Policy
		wantErr bool
	}{
		{name: "empty policy is valid (deny-all)", policy: Policy{}},
		{name: "issuer only", policy: Policy{Readers: []SubjectMatcher{{Issuer: "https://idp"}}}},
		{name: "sub_match only", policy: Policy{Readers: []SubjectMatcher{{SubMatch: "team-.*"}}}},
		{name: "email only", policy: Policy{Writers: []SubjectMatcher{{Email: "ci@example.com"}}}},
		{name: "claims only", policy: Policy{Readers: []SubjectMatcher{{ClaimsMatch: map[string]string{"plan": "pro"}}}}},
		{name: "explicit oidc kind", policy: Policy{Readers: []SubjectMatcher{{Kind: KindOIDC}}}},
		{name: "empty matcher rejected", policy: Policy{Readers: []SubjectMatcher{{}}}, wantErr: true},
		{name: "basictoken kind reserved", policy: Policy{Readers: []SubjectMatcher{{Issuer: "x", Kind: KindBasicToken}}}, wantErr: true},
		{name: "anonymous kind accepted", policy: Policy{Readers: []SubjectMatcher{{Issuer: "anonymous", SubMatch: "anonymous", Kind: KindAnonymous}}}},
		{name: "unknown kind rejected", policy: Policy{Readers: []SubjectMatcher{{Issuer: "x", Kind: "saml"}}}, wantErr: true},
		{name: "bad sub regex rejected", policy: Policy{Readers: []SubjectMatcher{{SubMatch: "([a-z"}}}, wantErr: true},
		{name: "bad claims regex rejected", policy: Policy{Readers: []SubjectMatcher{{ClaimsMatch: map[string]string{"k": "([a-z"}}}}, wantErr: true},
		{name: "empty claim key rejected", policy: Policy{Readers: []SubjectMatcher{{ClaimsMatch: map[string]string{"": "v"}}}}, wantErr: true},
		{name: "error in writers reported", policy: Policy{Writers: []SubjectMatcher{{}}}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := compilePolicy(tc.policy)
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidPolicy) {
					t.Fatalf("compilePolicy() err = %v, want errors.Is ErrInvalidPolicy", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("compilePolicy() unexpected err: %v", err)
			}
		})
	}
}

func oidcCtx(issuer, id, email string, claims map[string]any) *auth.AuthContext {
	return &auth.AuthContext{Issuer: issuer, ID: id, Email: email, Claims: claims, Kind: "oidc"}
}

func TestPolicyAuthorize(t *testing.T) {
	t.Parallel()

	policy := Policy{
		Readers: []SubjectMatcher{
			{Issuer: "https://idp", SubMatch: "team-.*"},
			{ClaimsMatch: map[string]string{"level": "42"}},
		},
		Writers: []SubjectMatcher{
			{Email: "ci@example.com"},
		},
	}
	compiled, err := compilePolicy(policy)
	if err != nil {
		t.Fatalf("compilePolicy: %v", err)
	}

	cases := []struct {
		name    string
		ac      *auth.AuthContext
		op      auth.Op
		wantErr error // nil means allowed
	}{
		{name: "nil ac read", ac: nil, op: auth.OpRead, wantErr: auth.ErrUnauthorized},
		{name: "nil ac write", ac: nil, op: auth.OpWrite, wantErr: auth.ErrUnauthorized},
		{name: "unknown op", ac: oidcCtx("https://idp", "team-a", "", nil), op: auth.Op("delete"), wantErr: auth.ErrUnknownOp},
		{name: "reader issuer+sub match", ac: oidcCtx("https://idp", "team-a", "", nil), op: auth.OpRead},
		{name: "reader sub anchored mismatch", ac: oidcCtx("https://idp", "xteam-a", "", nil), op: auth.OpRead, wantErr: auth.ErrUnauthorized},
		{name: "reader wrong issuer", ac: oidcCtx("https://other", "team-a", "", nil), op: auth.OpRead, wantErr: auth.ErrUnauthorized},
		{name: "reader via non-string claim match", ac: oidcCtx("https://x", "y", "", map[string]any{"level": float64(42)}), op: auth.OpRead},
		{name: "reader claim mismatch", ac: oidcCtx("https://x", "y", "", map[string]any{"level": float64(7)}), op: auth.OpRead, wantErr: auth.ErrUnauthorized},
		{name: "reader missing claim", ac: oidcCtx("https://x", "y", "", nil), op: auth.OpRead, wantErr: auth.ErrUnauthorized},
		{name: "writer email match", ac: oidcCtx("https://idp", "anyone", "ci@example.com", nil), op: auth.OpWrite},
		{name: "reader cannot write (independent)", ac: oidcCtx("https://idp", "team-a", "", nil), op: auth.OpWrite, wantErr: auth.ErrUnauthorized},
		{name: "writer cannot read (independent)", ac: oidcCtx("https://idp", "x", "ci@example.com", nil), op: auth.OpRead, wantErr: auth.ErrUnauthorized},
		{name: "anonymous kind never matches oidc matcher", ac: &auth.AuthContext{Issuer: "https://idp", ID: "team-a", Kind: "anonymous"}, op: auth.OpRead, wantErr: auth.ErrUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := compiled.Authorize(t.Context(), tc.ac, tc.op)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("Authorize() = %v, want allowed", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Authorize() = %v, want errors.Is %v", err, tc.wantErr)
			}
		})
	}
}

func TestAnonymousPolicyAuthorize(t *testing.T) {
	t.Parallel()

	compiled, err := compilePolicy(Policy{
		Readers: []SubjectMatcher{{Issuer: "anonymous", SubMatch: "anonymous", Kind: KindAnonymous}},
		Writers: []SubjectMatcher{{Issuer: "anonymous", SubMatch: "anonymous", Kind: KindAnonymous}},
	})
	if err != nil {
		t.Fatalf("compilePolicy: %v", err)
	}
	ac := &auth.AuthContext{Issuer: "anonymous", ID: "anonymous", Kind: "anonymous"}
	if err := compiled.Authorize(t.Context(), ac, auth.OpRead); err != nil {
		t.Fatalf("anonymous read = %v, want allowed", err)
	}
	if err := compiled.Authorize(t.Context(), ac, auth.OpWrite); err != nil {
		t.Fatalf("anonymous write = %v, want allowed", err)
	}
}

func TestEmptyPolicyDenyAll(t *testing.T) {
	t.Parallel()

	compiled, err := compilePolicy(Policy{})
	if err != nil {
		t.Fatalf("compilePolicy: %v", err)
	}
	ac := oidcCtx("https://idp", "anyone", "", nil)
	if err := compiled.Authorize(t.Context(), ac, auth.OpRead); !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("read on empty policy = %v, want ErrUnauthorized", err)
	}
	if err := compiled.Authorize(t.Context(), ac, auth.OpWrite); !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("write on empty policy = %v, want ErrUnauthorized", err)
	}
}
