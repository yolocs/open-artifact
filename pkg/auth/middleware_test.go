package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// fakeAuthn returns a fixed AuthContext or error.
type fakeAuthn struct {
	ac  *AuthContext
	err error
}

func (f fakeAuthn) Authenticate(*http.Request) (*AuthContext, error) { return f.ac, f.err }

func TestMiddlewareSuccessAttachesContext(t *testing.T) {
	t.Parallel()

	want := &AuthContext{Issuer: "https://issuer", ID: "user-1", Kind: "oidc"}
	var got *AuthContext
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = FromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})

	rec := httptest.NewRecorder()
	Middleware(fakeAuthn{ac: want})(next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("AuthContext mismatch (-want +got):\n%s", diff)
	}
}

func TestMiddlewareChallenges(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "no credential", err: ErrNoCredential},
		{name: "invalid token", err: ErrInvalidToken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			called := false
			next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })

			rec := httptest.NewRecorder()
			Middleware(fakeAuthn{err: tc.err})(next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

			if called {
				t.Error("next handler was called on auth failure")
			}
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
			challenges := rec.Header().Values("WWW-Authenticate")
			want := []string{`Bearer realm="open-artifact"`, `Basic realm="open-artifact"`}
			if diff := cmp.Diff(want, challenges); diff != "" {
				t.Errorf("WWW-Authenticate mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestAlwaysAnonymous(t *testing.T) {
	t.Parallel()

	ac, err := AlwaysAnonymous{}.Authenticate(httptest.NewRequest(http.MethodGet, "/x", nil))
	if err != nil {
		t.Fatalf("Authenticate() err = %v", err)
	}
	want := &AuthContext{Issuer: "anonymous", ID: "anonymous", Kind: "anonymous"}
	if diff := cmp.Diff(want, ac); diff != "" {
		t.Errorf("anonymous AuthContext mismatch (-want +got):\n%s", diff)
	}
}
