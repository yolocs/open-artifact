package command

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yolocs/open-artifact/pkg/auth"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestBuildAuthenticatorDisabled(t *testing.T) {
	t.Parallel()

	authn := buildAuthenticator(&runtimeConfig{DisableAuthn: true}, discardLogger())
	if _, ok := authn.(auth.AlwaysAnonymous); !ok {
		t.Fatalf("buildAuthenticator(disabled) = %T, want auth.AlwaysAnonymous", authn)
	}
	ac, err := authn.Authenticate(httptest.NewRequest(http.MethodGet, "/x", nil))
	if err != nil {
		t.Fatalf("Authenticate() err = %v", err)
	}
	if ac.Kind != "anonymous" {
		t.Errorf("Kind = %q, want anonymous", ac.Kind)
	}
}

func TestBuildAuthenticatorOIDCChain(t *testing.T) {
	t.Parallel()

	cfg := &runtimeConfig{
		AuthnKind:         "oidc",
		AuthnOIDCIssuers:  []string{"https://a.example", "https://b.example"},
		AuthnOIDCAudience: "open-artifact",
	}
	authn := buildAuthenticator(cfg, discardLogger())

	// No credential presented: the chain falls through every issuer and reports
	// ErrNoCredential without any network I/O (discovery is lazy).
	if _, err := authn.Authenticate(httptest.NewRequest(http.MethodGet, "/x", nil)); !errors.Is(err, auth.ErrNoCredential) {
		t.Fatalf("Authenticate(no creds) = %v, want ErrNoCredential", err)
	}
}
