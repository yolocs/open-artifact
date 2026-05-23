// Package auth is the frontend authentication and per-namespace authorization
// layer. Package clients authenticate to open-artifact with OIDC tokens
// presented as Bearer or Basic credentials; an Authenticator turns a request
// into an AuthContext, and an Authorizer decides whether that subject may read
// or write a namespace.
//
// The package is deliberately small and concern-free below it: it knows about
// HTTP requests and credentials, not about namespaces, storage, or formats.
// Namespace policy compilation and enforcement compose this package's
// AuthContext/Op/Authorizer around the namespace catalog (see pkg/namespace).
package auth

import (
	"context"
	"errors"
	"net/http"
)

// Sentinel errors. Every caller matches them with errors.Is.
var (
	// ErrNoCredential means the request carried no usable OIDC credential. A
	// chain treats it as "try the next authenticator"; middleware maps it to
	// 401.
	ErrNoCredential = errors.New("auth: no credential")
	// ErrInvalidToken means a credential was present but failed verification
	// (wrong audience, expired, bad signature, unknown key, or malformed).
	ErrInvalidToken = errors.New("auth: invalid token")
	// ErrUnauthorized means an authenticated subject is not permitted the
	// requested operation on the namespace.
	ErrUnauthorized = errors.New("auth: unauthorized")
	// ErrUnknownOp means an Authorizer was asked about an operation it does not
	// recognize.
	ErrUnknownOp = errors.New("auth: unknown op")
)

// Authenticator turns an inbound request into an AuthContext. A nil error and a
// non-nil AuthContext means success. ErrNoCredential means no usable credential
// was presented; ErrInvalidToken means a credential was present but failed
// verification.
type Authenticator interface {
	Authenticate(*http.Request) (*AuthContext, error)
}

// AuthContext is the verified identity of a caller. Kind names the credential
// family that produced it ("oidc" in v1, "anonymous" when authn is disabled).
type AuthContext struct {
	Issuer string         `json:"issuer"`
	ID     string         `json:"id"`
	Email  string         `json:"email,omitempty"`
	Claims map[string]any `json:"claims,omitempty"`
	Kind   string         `json:"kind"`
}

// Op is a coarse-grained operation an Authorizer reasons about.
type Op string

const (
	// OpRead covers every read of namespace contents.
	OpRead Op = "read"
	// OpWrite covers every mutation of namespace contents.
	OpWrite Op = "write"
)

// Authorizer decides whether a subject may perform an operation. It returns nil
// to allow, ErrUnauthorized to deny, and ErrUnknownOp for an operation it does
// not recognize.
type Authorizer interface {
	Authorize(ctx context.Context, ac *AuthContext, op Op) error
}

// contextKey is the unexported type for the AuthContext context key.
type contextKey struct{}

// NewContext returns a copy of ctx carrying ac.
func NewContext(ctx context.Context, ac *AuthContext) context.Context {
	return context.WithValue(ctx, contextKey{}, ac)
}

// FromContext returns the AuthContext attached by Middleware, or nil if none.
func FromContext(ctx context.Context) *AuthContext {
	ac, _ := ctx.Value(contextKey{}).(*AuthContext)
	return ac
}
