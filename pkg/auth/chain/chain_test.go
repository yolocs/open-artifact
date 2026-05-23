package chain

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yolocs/open-artifact/pkg/auth"
)

// stub records whether it was called and returns a fixed result.
type stub struct {
	id     string
	err    error
	called *bool
}

func (s stub) Authenticate(*http.Request) (*auth.AuthContext, error) {
	if s.called != nil {
		*s.called = true
	}
	if s.err != nil {
		return nil, s.err
	}
	return &auth.AuthContext{ID: s.id, Kind: "oidc"}, nil
}

func req() *http.Request { return httptest.NewRequest(http.MethodGet, "/x", nil) }

func TestChainEmpty(t *testing.T) {
	t.Parallel()
	if _, err := New().Authenticate(req()); !errors.Is(err, auth.ErrNoCredential) {
		t.Fatalf("empty chain = %v, want ErrNoCredential", err)
	}
}

func TestChainFallthroughToSuccess(t *testing.T) {
	t.Parallel()

	thirdCalled := false
	c := New(
		stub{err: auth.ErrNoCredential},
		stub{id: "winner"},
		stub{id: "third", called: &thirdCalled},
	)
	ac, err := c.Authenticate(req())
	if err != nil {
		t.Fatalf("Authenticate() err = %v", err)
	}
	if ac.ID != "winner" {
		t.Errorf("ID = %q, want winner (first success wins)", ac.ID)
	}
	if thirdCalled {
		t.Error("authenticator after the first success was called")
	}
}

func TestChainStopsOnInvalidToken(t *testing.T) {
	t.Parallel()

	secondCalled := false
	c := New(
		stub{err: auth.ErrInvalidToken},
		stub{id: "unreached", called: &secondCalled},
	)
	_, err := c.Authenticate(req())
	if !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("Authenticate() err = %v, want ErrInvalidToken", err)
	}
	if secondCalled {
		t.Error("chain continued past a hard error")
	}
}

func TestChainAllNoCredential(t *testing.T) {
	t.Parallel()

	c := New(stub{err: auth.ErrNoCredential}, stub{err: auth.ErrNoCredential})
	if _, err := c.Authenticate(req()); !errors.Is(err, auth.ErrNoCredential) {
		t.Fatalf("Authenticate() err = %v, want ErrNoCredential", err)
	}
}
