package httpclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGetSuccess(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"abc"`)
		w.Header().Set("Last-Modified", "Wed, 21 Oct 2026 07:28:00 GMT")
		w.Header().Set("Cache-Control", "max-age=300")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := New(WithHTTPClient(srv.Client()))
	resp, err := c.Get(t.Context(), srv.URL)
	if err != nil {
		t.Fatalf("Get() = %v", err)
	}
	if !resp.IsOK() {
		t.Fatalf("IsOK() = false, status %d", resp.Status)
	}
	if got, want := string(resp.Body), `{"ok":true}`; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
	if resp.ContentType != "application/json" {
		t.Fatalf("ContentType = %q", resp.ContentType)
	}
	if resp.ETag != `"abc"` {
		t.Fatalf("ETag = %q", resp.ETag)
	}
	if resp.LastModified == "" {
		t.Fatalf("LastModified empty")
	}
	if resp.CacheControl != "max-age=300" {
		t.Fatalf("CacheControl = %q", resp.CacheControl)
	}
}

func TestGetStatusMapping(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		status     int
		wantOK     bool
		wantNF     bool
		wantSrvErr bool
	}{
		{name: "ok", status: 200, wantOK: true},
		{name: "not found", status: 404, wantNF: true},
		{name: "server error", status: 503, wantSrvErr: true},
		{name: "forbidden", status: 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			c := New(WithHTTPClient(srv.Client()))
			resp, err := c.Get(t.Context(), srv.URL)
			if err != nil {
				t.Fatalf("Get() = %v (HTTP status is not an error)", err)
			}
			if resp.IsOK() != tc.wantOK {
				t.Fatalf("IsOK() = %v, want %v", resp.IsOK(), tc.wantOK)
			}
			if resp.IsNotFound() != tc.wantNF {
				t.Fatalf("IsNotFound() = %v, want %v", resp.IsNotFound(), tc.wantNF)
			}
			if resp.IsServerError() != tc.wantSrvErr {
				t.Fatalf("IsServerError() = %v, want %v", resp.IsServerError(), tc.wantSrvErr)
			}
		})
	}
}

func TestGetOversized(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 1000)))
	}))
	defer srv.Close()

	c := New(WithHTTPClient(srv.Client()), WithMaxBodyBytes(100))
	_, err := c.Get(t.Context(), srv.URL)
	if !errors.Is(err, ErrOversized) {
		t.Fatalf("Get() = %v, want ErrOversized", err)
	}
}

func TestGetExactCap(t *testing.T) {
	t.Parallel()

	body := strings.Repeat("y", 100)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := New(WithHTTPClient(srv.Client()), WithMaxBodyBytes(100))
	resp, err := c.Get(t.Context(), srv.URL)
	if err != nil {
		t.Fatalf("Get() at exact cap = %v, want nil", err)
	}
	if string(resp.Body) != body {
		t.Fatalf("body length = %d, want 100", len(resp.Body))
	}
}

func TestHead(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("method = %s, want HEAD", r.Method)
		}
		w.Header().Set("ETag", `"v1"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("should-not-be-read"))
	}))
	defer srv.Close()

	c := New(WithHTTPClient(srv.Client()))
	resp, err := c.Head(t.Context(), srv.URL)
	if err != nil {
		t.Fatalf("Head() = %v", err)
	}
	if !resp.IsOK() {
		t.Fatalf("IsOK() = false")
	}
	if resp.Body != nil {
		t.Fatalf("Head() Body = %q, want nil", resp.Body)
	}
	if resp.ETag != `"v1"` {
		t.Fatalf("ETag = %q", resp.ETag)
	}
}

func TestContextCancellation(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	c := New(WithHTTPClient(srv.Client()))
	_, err := c.Get(ctx, srv.URL)
	if err == nil {
		t.Fatal("Get() = nil, want context error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Get() error = %v, want context.DeadlineExceeded", err)
	}
}

func TestInjectedClientIsUsed(t *testing.T) {
	t.Parallel()

	var seen string
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		seen = req.URL.String()
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{},
			Body:       http.NoBody,
		}, nil
	})
	c := New(WithHTTPClient(&http.Client{Transport: rt}))
	if _, err := c.Get(t.Context(), "https://upstream.invalid/path"); err != nil {
		t.Fatalf("Get() = %v", err)
	}
	if seen != "https://upstream.invalid/path" {
		t.Fatalf("injected transport saw %q", seen)
	}
}

func TestDefaultTransport(t *testing.T) {
	t.Parallel()

	c := New()
	tr, ok := c.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("default Transport = %T, want *http.Transport", c.http.Transport)
	}
	if !tr.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 = false, want true (assume HTTP/2 over TLS)")
	}
	if tr.MaxIdleConnsPerHost <= 2 {
		t.Errorf("MaxIdleConnsPerHost = %d, want a raised pool for the single upstream host", tr.MaxIdleConnsPerHost)
	}
	if tr.TLSHandshakeTimeout == 0 || tr.ResponseHeaderTimeout == 0 || tr.IdleConnTimeout == 0 {
		t.Errorf("expected non-zero transport timeouts, got TLS=%v header=%v idle=%v",
			tr.TLSHandshakeTimeout, tr.ResponseHeaderTimeout, tr.IdleConnTimeout)
	}
	// No blanket client timeout — overall deadlines are the caller's via context.
	if c.http.Timeout != 0 {
		t.Errorf("client.Timeout = %v, want 0 (use request context)", c.http.Timeout)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
