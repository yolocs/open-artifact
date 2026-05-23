package auth

import (
	"encoding/base64"
	"net/http"
	"strings"
)

// sentinelUsers are the Basic-auth usernames that mark the password as an OIDC
// token rather than a real password. A Basic header with any other username is
// not a password login open-artifact understands, so it is treated as no
// credential.
var sentinelUsers = map[string]bool{
	"_oidc":             true,
	"oauth2accesstoken": true,
	"_token":            true,
}

// ExtractToken pulls a bearer token out of a request, supporting two wire
// presentations:
//
//	Authorization: Bearer <token>
//	Authorization: Basic base64("<sentinel-user>:<token>")
//
// A Basic header whose username is not a sentinel is not a password login and
// yields ErrNoCredential. An empty or absent token is ErrNoCredential, never
// anonymous success.
func ExtractToken(r *http.Request) (string, error) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", ErrNoCredential
	}
	scheme, rest, ok := strings.Cut(h, " ")
	if !ok {
		return "", ErrNoCredential
	}
	rest = strings.TrimSpace(rest)
	switch {
	case strings.EqualFold(scheme, "bearer"):
		if rest == "" {
			return "", ErrNoCredential
		}
		return rest, nil
	case strings.EqualFold(scheme, "basic"):
		return tokenFromBasic(rest)
	default:
		return "", ErrNoCredential
	}
}

// tokenFromBasic decodes a Basic credential and returns the password when the
// username is a recognized sentinel. Anything else is ErrNoCredential.
func tokenFromBasic(encoded string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", ErrNoCredential
	}
	user, pass, ok := strings.Cut(string(raw), ":")
	if !ok || !sentinelUsers[user] || pass == "" {
		return "", ErrNoCredential
	}
	return pass, nil
}
