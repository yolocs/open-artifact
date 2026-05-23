// Package chain composes several authenticators into one. It backs the
// multi-issuer OIDC configuration: each configured issuer is its own
// authenticator, and the chain tries them in order.
package chain

import (
	"errors"
	"net/http"

	"github.com/yolocs/open-artifact/pkg/auth"
)

// chain tries its children in declaration order. ErrNoCredential from a child
// falls through to the next; the first success wins; the first error that is
// not ErrNoCredential stops the chain and is returned.
type chain struct {
	children []auth.Authenticator
}

// New returns an Authenticator that tries children in order. An empty chain
// always returns ErrNoCredential.
func New(children ...auth.Authenticator) auth.Authenticator {
	return &chain{children: children}
}

func (c *chain) Authenticate(r *http.Request) (*auth.AuthContext, error) {
	for _, child := range c.children {
		ac, err := child.Authenticate(r)
		if err == nil {
			return ac, nil
		}
		if errors.Is(err, auth.ErrNoCredential) {
			continue
		}
		return nil, err
	}
	return nil, auth.ErrNoCredential
}
