package oidc

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/yolocs/open-artifact/pkg/auth"
)

// fakeIssuer is an in-process OIDC issuer: it serves discovery and JWKS and
// mints signed tokens. Its signing key can be rotated.
type fakeIssuer struct {
	server *httptest.Server
	issuer string

	mu    sync.Mutex
	priv  *rsa.PrivateKey
	keyID string
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	fi := &fakeIssuer{}
	fi.rotate(t, "key-1")

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                fi.issuer,
			"jwks_uri":                              fi.issuer + "/jwks",
			"authorization_endpoint":                fi.issuer + "/auth",
			"token_endpoint":                        fi.issuer + "/token",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		fi.mu.Lock()
		defer fi.mu.Unlock()
		jwks := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key:       fi.priv.Public(),
			KeyID:     fi.keyID,
			Algorithm: "RS256",
			Use:       "sig",
		}}}
		_ = json.NewEncoder(w).Encode(jwks)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	fi.server = srv
	fi.issuer = srv.URL
	return fi
}

func (fi *fakeIssuer) rotate(t *testing.T, keyID string) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	fi.mu.Lock()
	fi.priv = priv
	fi.keyID = keyID
	fi.mu.Unlock()
}

// sign mints a token from claims using the issuer's current key.
func (fi *fakeIssuer) sign(t *testing.T, claims map[string]any) string {
	fi.mu.Lock()
	priv, kid := fi.priv, fi.keyID
	fi.mu.Unlock()
	return signWith(t, priv, kid, claims)
}

func signWith(t *testing.T, priv *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: priv},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid),
	)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	obj, err := signer.Sign(payload)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	s, err := obj.CompactSerialize()
	if err != nil {
		t.Fatalf("CompactSerialize: %v", err)
	}
	return s
}

// standardClaims returns a baseline valid claim set for fi/audience.
func (fi *fakeIssuer) standardClaims(audience string) map[string]any {
	return map[string]any{
		"iss": fi.issuer,
		"sub": "user-123",
		"aud": audience,
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Unix(),
	}
}

func bearer(token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

const testAudience = "open-artifact"

func TestAuthenticateValidToken(t *testing.T) {
	t.Parallel()

	fi := newFakeIssuer(t)
	a := New(fi.issuer, testAudience, WithHTTPClient(fi.server.Client()))

	claims := fi.standardClaims(testAudience)
	claims["groups"] = []any{"eng", "release"}
	ac, err := a.Authenticate(bearer(fi.sign(t, claims)))
	if err != nil {
		t.Fatalf("Authenticate() err = %v", err)
	}
	if ac.Issuer != fi.issuer {
		t.Errorf("Issuer = %q, want %q", ac.Issuer, fi.issuer)
	}
	if ac.ID != "user-123" {
		t.Errorf("ID = %q, want user-123", ac.ID)
	}
	if ac.Kind != "oidc" {
		t.Errorf("Kind = %q, want oidc", ac.Kind)
	}
	if _, ok := ac.Claims["groups"]; !ok {
		t.Errorf("Claims missing custom 'groups': %v", ac.Claims)
	}
}

func TestAuthenticateWrongAudience(t *testing.T) {
	t.Parallel()

	fi := newFakeIssuer(t)
	a := New(fi.issuer, testAudience, WithHTTPClient(fi.server.Client()))

	_, err := a.Authenticate(bearer(fi.sign(t, fi.standardClaims("someone-else"))))
	if !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("Authenticate() err = %v, want ErrInvalidToken", err)
	}
}

func TestAuthenticateWrongIssuerFallsThrough(t *testing.T) {
	t.Parallel()

	fi := newFakeIssuer(t)
	a := New(fi.issuer, testAudience, WithHTTPClient(fi.server.Client()))

	claims := fi.standardClaims(testAudience)
	claims["iss"] = "https://attacker.example"
	_, err := a.Authenticate(bearer(fi.sign(t, claims)))
	if !errors.Is(err, auth.ErrNoCredential) {
		t.Fatalf("Authenticate() err = %v, want ErrNoCredential (fall-through)", err)
	}
}

func TestAuthenticateMissingIssuer(t *testing.T) {
	t.Parallel()

	fi := newFakeIssuer(t)
	a := New(fi.issuer, testAudience, WithHTTPClient(fi.server.Client()))

	claims := fi.standardClaims(testAudience)
	delete(claims, "iss")
	_, err := a.Authenticate(bearer(fi.sign(t, claims)))
	if !errors.Is(err, auth.ErrNoCredential) {
		t.Fatalf("Authenticate() err = %v, want ErrNoCredential", err)
	}
}

func TestAuthenticateBadSignature(t *testing.T) {
	t.Parallel()

	fi := newFakeIssuer(t)
	a := New(fi.issuer, testAudience, WithHTTPClient(fi.server.Client()))

	// Sign with a key the issuer does not publish, reusing the published kid.
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	token := signWith(t, other, "key-1", fi.standardClaims(testAudience))
	if _, err := a.Authenticate(bearer(token)); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("Authenticate() err = %v, want ErrInvalidToken", err)
	}
}

func TestAuthenticateExpiredToken(t *testing.T) {
	t.Parallel()

	fi := newFakeIssuer(t)
	a := New(fi.issuer, testAudience, WithHTTPClient(fi.server.Client()))

	claims := fi.standardClaims(testAudience)
	claims["exp"] = time.Now().Add(-time.Hour).Unix()
	if _, err := a.Authenticate(bearer(fi.sign(t, claims))); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("Authenticate() err = %v, want ErrInvalidToken", err)
	}
}

func TestAuthenticateKeyRotation(t *testing.T) {
	t.Parallel()

	fi := newFakeIssuer(t)
	a := New(fi.issuer, testAudience, WithHTTPClient(fi.server.Client()))

	// Prime discovery and JWKS with the original key.
	if _, err := a.Authenticate(bearer(fi.sign(t, fi.standardClaims(testAudience)))); err != nil {
		t.Fatalf("Authenticate(before rotation) err = %v", err)
	}

	// Rotate to a new key and mint a fresh token; the verifier must refetch the
	// JWKS to discover the new key.
	fi.rotate(t, "key-2")
	if _, err := a.Authenticate(bearer(fi.sign(t, fi.standardClaims(testAudience)))); err != nil {
		t.Fatalf("Authenticate(after rotation) err = %v", err)
	}
}

func TestAuthenticateEmailVerification(t *testing.T) {
	t.Parallel()

	fi := newFakeIssuer(t)
	a := New(fi.issuer, testAudience, WithHTTPClient(fi.server.Client()))

	t.Run("verified", func(t *testing.T) {
		claims := fi.standardClaims(testAudience)
		claims["email"] = "dev@example.com"
		claims["email_verified"] = true
		ac, err := a.Authenticate(bearer(fi.sign(t, claims)))
		if err != nil {
			t.Fatalf("Authenticate() err = %v", err)
		}
		if ac.Email != "dev@example.com" {
			t.Errorf("Email = %q, want dev@example.com", ac.Email)
		}
	})

	t.Run("unverified", func(t *testing.T) {
		claims := fi.standardClaims(testAudience)
		claims["email"] = "dev@example.com"
		claims["email_verified"] = false
		ac, err := a.Authenticate(bearer(fi.sign(t, claims)))
		if err != nil {
			t.Fatalf("Authenticate() err = %v", err)
		}
		if ac.Email != "" {
			t.Errorf("Email = %q, want empty for unverified email", ac.Email)
		}
	})
}

func TestAuthenticateNoCredential(t *testing.T) {
	t.Parallel()

	fi := newFakeIssuer(t)
	a := New(fi.issuer, testAudience, WithHTTPClient(fi.server.Client()))

	if _, err := a.Authenticate(httptest.NewRequest(http.MethodGet, "/x", nil)); !errors.Is(err, auth.ErrNoCredential) {
		t.Fatalf("Authenticate(no header) err = %v, want ErrNoCredential", err)
	}
}

func TestAuthenticateOversizedToken(t *testing.T) {
	t.Parallel()

	fi := newFakeIssuer(t)
	a := New(fi.issuer, testAudience, WithHTTPClient(fi.server.Client()))

	huge := "a." + base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("x", maxTokenSize))) + ".c"
	if _, err := a.Authenticate(bearer(huge)); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("Authenticate(oversized) err = %v, want ErrInvalidToken", err)
	}
}

func TestPeekIssuer(t *testing.T) {
	t.Parallel()

	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"https://x"}`))
	iss, ok := peekIssuer("h." + payload + ".s")
	if !ok || iss != "https://x" {
		t.Errorf("peekIssuer = (%q, %v), want (https://x, true)", iss, ok)
	}
	if _, ok := peekIssuer("not-a-jwt"); ok {
		t.Error("peekIssuer(not-a-jwt) ok = true, want false")
	}
	if _, ok := peekIssuer("a.b.c"); ok {
		t.Error("peekIssuer(non-base64 payload) ok = true, want false")
	}
}
