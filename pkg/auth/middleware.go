package auth

import (
	"encoding/json"
	"errors"
	"net/http"
)

// realm is the WWW-Authenticate realm advertised on 401 responses.
const realm = "open-artifact"

// Middleware authenticates every request with authn before calling next. On
// success it attaches the AuthContext to the request context. On a missing
// credential or an invalid token it writes 401 with both Bearer and Basic
// challenges and does not call next.
//
// It must not be installed around /healthz, /readyz, or /metrics; it is wired
// only inside format routes.
func Middleware(authn Authenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ac, err := authn.Authenticate(r)
			if err != nil {
				writeChallenge(w, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(NewContext(r.Context(), ac)))
		})
	}
}

// writeChallenge writes a 401 carrying both authentication challenges so a
// client may retry with either presentation.
func writeChallenge(w http.ResponseWriter, err error) {
	w.Header().Add("WWW-Authenticate", `Bearer realm="`+realm+`"`)
	w.Header().Add("WWW-Authenticate", `Basic realm="`+realm+`"`)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	msg := "missing credential"
	if errors.Is(err, ErrInvalidToken) {
		msg = "invalid token"
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// AlwaysAnonymous is an Authenticator that authenticates every request as the
// anonymous subject. It is installed only when authn is explicitly disabled.
type AlwaysAnonymous struct{}

// Authenticate returns the fixed anonymous AuthContext and never fails.
func (AlwaysAnonymous) Authenticate(*http.Request) (*AuthContext, error) {
	return &AuthContext{Issuer: "anonymous", ID: "anonymous", Kind: "anonymous"}, nil
}
