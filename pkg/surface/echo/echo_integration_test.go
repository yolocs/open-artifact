//go:build integration

package echo_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"gocloud.dev/blob"
	"gocloud.dev/blob/memblob"

	"github.com/yolocs/open-artifact/pkg/auth/chain"
	"github.com/yolocs/open-artifact/pkg/auth/oidc"
	"github.com/yolocs/open-artifact/pkg/namespace"
	"github.com/yolocs/open-artifact/pkg/surface/echo"
)

const audience = "open-artifact"

// fakeIssuer is an in-process OIDC issuer serving discovery + JWKS and minting
// signed tokens, mirroring a real provider closely enough for an e2e test.
type fakeIssuer struct {
	server *httptest.Server
	issuer string
	priv   *rsa.PrivateKey
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	fi := &fakeIssuer{priv: priv}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                fi.issuer,
			"jwks_uri":                              fi.issuer + "/jwks",
			"authorization_endpoint":                fi.issuer + "/auth",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key:       fi.priv.Public(),
			KeyID:     "key-1",
			Algorithm: "RS256",
			Use:       "sig",
		}}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	fi.server = srv
	fi.issuer = srv.URL
	return fi
}

func (fi *fakeIssuer) token(t *testing.T, sub string) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: fi.priv},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "key-1"),
	)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	payload, _ := json.Marshal(map[string]any{
		"iss": fi.issuer,
		"sub": sub,
		"aud": audience,
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Unix(),
	})
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

func memBucket(t *testing.T) *blob.Bucket {
	t.Helper()
	b := memblob.OpenBucket(nil)
	t.Cleanup(func() { b.Close() })
	return b
}

// TestEchoEndToEnd drives the whole front-door stack — credential extraction,
// OIDC verification against a live (fake) issuer, the 401 challenge, and
// per-namespace read/write authorization — through the echo surface.
func TestEchoEndToEnd(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	fi := newFakeIssuer(t)
	b := memBucket(t)

	catalog, err := namespace.NewStore(b, "")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	reg, err := namespace.NewRegistry(b, "", catalog)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	// ci-reader grants the CI subject read; ci-deny grants nothing.
	if _, err := catalog.Put(ctx, &namespace.Namespace{Name: "ci-reader", Spec: namespace.Spec{
		Policy: namespace.Policy{Readers: []namespace.SubjectMatcher{{Issuer: fi.issuer, SubMatch: "repo:.*"}}},
	}}); err != nil {
		t.Fatalf("Put ci-reader: %v", err)
	}
	if _, err := catalog.Put(ctx, &namespace.Namespace{Name: "ci-deny"}); err != nil {
		t.Fatalf("Put ci-deny: %v", err)
	}

	authn := chain.New(oidc.New(fi.issuer, audience, oidc.WithHTTPClient(fi.server.Client())))
	srv := httptest.NewServer(echo.Handler(reg, authn, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(srv.Close)

	good := fi.token(t, "repo:yolocs/open-artifact:ref:refs/heads/main")

	cases := []struct {
		name       string
		method     string
		path       string
		authHeader string
		want       int
	}{
		{name: "read allowed", method: http.MethodGet, path: "/ci-reader/echo", authHeader: "Bearer " + good, want: http.StatusOK},
		{name: "no credential", method: http.MethodGet, path: "/ci-reader/echo", want: http.StatusUnauthorized},
		{name: "invalid token", method: http.MethodGet, path: "/ci-reader/echo", authHeader: "Bearer not.a.jwt", want: http.StatusUnauthorized},
		{name: "write denied for reader", method: http.MethodPut, path: "/ci-reader/echo", authHeader: "Bearer " + good, want: http.StatusForbidden},
		{name: "deny-all namespace", method: http.MethodGet, path: "/ci-deny/echo", authHeader: "Bearer " + good, want: http.StatusForbidden},
		{name: "unknown namespace", method: http.MethodGet, path: "/ci-ghost/echo", authHeader: "Bearer " + good, want: http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(ctx, tc.method, srv.URL+tc.path, nil)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			if tc.authHeader != "" {
				req.Header.Set("Authorization", tc.authHeader)
			}
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d, want %d (body: %s)", resp.StatusCode, tc.want, body)
			}
			if tc.want == http.StatusUnauthorized {
				if got := resp.Header.Values("WWW-Authenticate"); len(got) != 2 {
					t.Errorf("WWW-Authenticate = %v, want Bearer and Basic challenges", got)
				}
			}
		})
	}
}
