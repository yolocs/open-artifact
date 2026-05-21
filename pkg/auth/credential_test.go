package auth

import (
	"encoding/base64"
	"errors"
	"net/http"
	"testing"
)

func basicHeader(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

func TestExtractToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		header    string
		wantToken string
		wantErr   error
	}{
		{name: "bearer", header: "Bearer abc.def.ghi", wantToken: "abc.def.ghi"},
		{name: "bearer case-insensitive scheme", header: "bearer tok", wantToken: "tok"},
		{name: "bearer trims spaces", header: "Bearer   tok  ", wantToken: "tok"},
		{name: "bearer empty token", header: "Bearer ", wantErr: ErrNoCredential},
		{name: "basic _oidc", header: basicHeader("_oidc", "tok"), wantToken: "tok"},
		{name: "basic oauth2accesstoken", header: basicHeader("oauth2accesstoken", "tok"), wantToken: "tok"},
		{name: "basic _token", header: basicHeader("_token", "tok"), wantToken: "tok"},
		{name: "basic token with colons", header: basicHeader("_token", "a:b:c"), wantToken: "a:b:c"},
		{name: "basic non-sentinel user", header: basicHeader("alice", "hunter2"), wantErr: ErrNoCredential},
		{name: "basic empty password", header: basicHeader("_oidc", ""), wantErr: ErrNoCredential},
		{name: "basic bad base64", header: "Basic !!!notbase64!!!", wantErr: ErrNoCredential},
		{name: "basic no colon", header: "Basic " + base64.StdEncoding.EncodeToString([]byte("nocolon")), wantErr: ErrNoCredential},
		{name: "no header", header: "", wantErr: ErrNoCredential},
		{name: "unknown scheme", header: "Token abc", wantErr: ErrNoCredential},
		{name: "no space", header: "Bearer", wantErr: ErrNoCredential},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptestRequest(tc.header)
			got, err := ExtractToken(r)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ExtractToken() err = %v, want errors.Is %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ExtractToken() unexpected err: %v", err)
			}
			if got != tc.wantToken {
				t.Errorf("ExtractToken() = %q, want %q", got, tc.wantToken)
			}
		})
	}
}

func httptestRequest(authHeader string) *http.Request {
	r, _ := http.NewRequest(http.MethodGet, "http://example/", nil)
	if authHeader != "" {
		r.Header.Set("Authorization", authHeader)
	}
	return r
}
