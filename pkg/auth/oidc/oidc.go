// Package oidc is an OIDC token authenticator. It verifies bearer tokens issued
// by a single configured issuer and audience using OIDC discovery and JWKS,
// both fetched lazily on first use. Multiple issuers are supported by composing
// several authenticators with pkg/auth/chain.
package oidc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	coreoidc "github.com/coreos/go-oidc/v3/oidc"

	"github.com/yolocs/open-artifact/pkg/auth"
)

const (
	// maxTokenSize bounds an accepted bearer token to keep peeking and
	// verification from doing unbounded work on abusive input.
	maxTokenSize = 1 << 16 // 64 KiB
	// maxResponseSize bounds discovery and JWKS responses fetched from the
	// issuer so a hostile or broken endpoint cannot exhaust memory.
	maxResponseSize = 1 << 20 // 1 MiB
)

// Authenticator verifies OIDC tokens for one issuer/audience pair. Discovery
// and the verifier are built lazily on first Authenticate and reused; a failed
// build is not cached, so a transient discovery outage is retried.
type Authenticator struct {
	issuer     string
	audience   string
	httpClient *http.Client

	mu       sync.Mutex
	verifier *coreoidc.IDTokenVerifier
}

// Option customizes an Authenticator at construction.
type Option func(*Authenticator)

// WithHTTPClient overrides the HTTP client used for discovery and JWKS fetches.
// Tests use it to point at a fake issuer.
func WithHTTPClient(c *http.Client) Option {
	return func(a *Authenticator) { a.httpClient = c }
}

// New constructs an Authenticator for the given issuer and audience.
func New(issuer, audience string, opts ...Option) *Authenticator {
	a := &Authenticator{issuer: issuer, audience: audience}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// Authenticate verifies the request's bearer token. A token for a different
// issuer (or with no usable issuer claim) yields ErrNoCredential so a chain can
// try the next authenticator. A token for this issuer that fails verification
// yields ErrInvalidToken.
func (a *Authenticator) Authenticate(r *http.Request) (*auth.AuthContext, error) {
	token, err := auth.ExtractToken(r)
	if err != nil {
		return nil, err
	}
	if len(token) > maxTokenSize {
		return nil, fmt.Errorf("%w: token exceeds size limit", auth.ErrInvalidToken)
	}

	// Peek the unverified issuer before any network work. A non-matching or
	// absent issuer is not our credential.
	if iss, ok := peekIssuer(token); !ok || iss != a.issuer {
		return nil, auth.ErrNoCredential
	}

	verifier, err := a.getVerifier(r.Context())
	if err != nil {
		return nil, err
	}

	idToken, err := verifier.Verify(r.Context(), token)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", auth.ErrInvalidToken, err)
	}

	var claims map[string]any
	if err := idToken.Claims(&claims); err != nil {
		return nil, fmt.Errorf("%w: decode claims: %v", auth.ErrInvalidToken, err)
	}

	ac := &auth.AuthContext{
		Issuer: idToken.Issuer,
		ID:     idToken.Subject,
		Claims: claims,
		Kind:   "oidc",
	}
	if verified, _ := claims["email_verified"].(bool); verified {
		if email, ok := claims["email"].(string); ok {
			ac.Email = email
		}
	}
	return ac, nil
}

// getVerifier returns the cached verifier, building it via discovery on first
// use. The build is guarded by a mutex and not memoized on failure.
func (a *Authenticator) getVerifier(ctx context.Context) (*coreoidc.IDTokenVerifier, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.verifier != nil {
		return a.verifier, nil
	}
	dctx := coreoidc.ClientContext(ctx, a.discoveryClient())
	provider, err := coreoidc.NewProvider(dctx, a.issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc: discovery for issuer %q: %w", a.issuer, err)
	}
	// ClientID drives audience verification; the verifier rejects "none" and
	// only accepts the provider's advertised signing algorithms (RS256 by
	// default), never "none".
	a.verifier = provider.Verifier(&coreoidc.Config{ClientID: a.audience})
	return a.verifier, nil
}

// discoveryClient returns the HTTP client used for discovery and JWKS, wrapped
// so every response body is size-capped.
func (a *Authenticator) discoveryClient() *http.Client {
	base := a.httpClient
	if base == nil {
		base = http.DefaultClient
	}
	rt := base.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	clone := *base
	clone.Transport = &cappingTransport{base: rt, limit: maxResponseSize}
	return &clone
}

// peekIssuer base64-decodes a compact JWS payload and returns its "iss" claim
// without verifying the signature. It reports ok=false for anything that is not
// a three-part JWT with a non-empty issuer.
func peekIssuer(token string) (string, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	var claims struct {
		Iss string `json:"iss"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", false
	}
	return claims.Iss, claims.Iss != ""
}

// cappingTransport wraps a RoundTripper to bound every response body.
type cappingTransport struct {
	base  http.RoundTripper
	limit int64
}

func (t *cappingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	resp.Body = &cappedBody{rc: resp.Body, remaining: t.limit}
	return resp, nil
}

// cappedBody errors once more than its limit has been read.
type cappedBody struct {
	rc        io.ReadCloser
	remaining int64
}

func (c *cappedBody) Read(p []byte) (int, error) {
	if c.remaining <= 0 {
		return 0, fmt.Errorf("oidc: response exceeds %d byte limit", maxResponseSize)
	}
	if int64(len(p)) > c.remaining {
		p = p[:c.remaining]
	}
	n, err := c.rc.Read(p)
	c.remaining -= int64(n)
	return n, err
}

func (c *cappedBody) Close() error { return c.rc.Close() }
