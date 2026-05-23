// Package auth defines authentication and authorization contracts shared by
// the data-plane surfaces.
package auth

import (
	"context"
	"errors"
	"net/http"
)

var (
	ErrNoCredential = errors.New("open-artifact auth: no credential")
	ErrInvalidToken = errors.New("open-artifact auth: invalid token")
	ErrUnauthorized = errors.New("open-artifact auth: unauthorized")
	ErrUnknownOp    = errors.New("open-artifact auth: unknown operation")
)

const (
	KindOIDC      = "oidc"
	KindAnonymous = "anonymous"

	AnonymousIssuer = "anonymous"
	AnonymousID     = "anonymous"
)

type Op string

const (
	OpRead  Op = "read"
	OpWrite Op = "write"
)

type Authenticator interface {
	Authenticate(*http.Request) (*AuthContext, error)
}

type Authorizer interface {
	Authorize(context.Context, *AuthContext, Op) error
}

type AuthContext struct {
	Issuer string         `json:"issuer"`
	ID     string         `json:"id"`
	Email  string         `json:"email,omitempty"`
	Claims map[string]any `json:"claims,omitempty"`
	Kind   string         `json:"kind"`
}

type contextKey struct{}

func ContextWithAuth(ctx context.Context, ac *AuthContext) context.Context {
	return context.WithValue(ctx, contextKey{}, ac)
}

func FromContext(ctx context.Context) (*AuthContext, bool) {
	ac, ok := ctx.Value(contextKey{}).(*AuthContext)
	return ac, ok
}

type anonymousAuthenticator struct{}

func AlwaysAnonymous() Authenticator {
	return anonymousAuthenticator{}
}

func (anonymousAuthenticator) Authenticate(*http.Request) (*AuthContext, error) {
	return &AuthContext{Issuer: AnonymousIssuer, ID: AnonymousID, Kind: KindAnonymous}, nil
}

func Middleware(authn Authenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ac, err := authn.Authenticate(r)
			if err == nil {
				next.ServeHTTP(w, r.WithContext(ContextWithAuth(r.Context(), ac)))
				return
			}
			if errors.Is(err, ErrNoCredential) || errors.Is(err, ErrInvalidToken) {
				w.Header().Add("WWW-Authenticate", `Bearer realm="open-artifact"`)
				w.Header().Add("WWW-Authenticate", `Basic realm="open-artifact"`)
				http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
				return
			}
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		})
	}
}
