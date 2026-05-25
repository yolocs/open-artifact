// Package httpclient is the proxy's upstream HTTP helper: context-aware GET and
// HEAD with a configurable body cap and status-mapping helpers. It carries no
// package-format behavior — callers (the format surfaces) interpret the bytes.
//
// The helper never turns an HTTP status into an error: a 404 or 500 is a valid
// response the caller must distinguish (cache a negative result, fall back to
// stale, etc.). Errors are reserved for transport failures, context
// cancellation, an oversized body, or a body read error.
package httpclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// DefaultMaxBodyBytes caps a buffered GET body when no override is configured.
// Metadata documents (PyPI simple pages, npm packuments) are comfortably under
// this; format code that streams large artifacts should set its own cap.
const DefaultMaxBodyBytes int64 = 64 << 20 // 64 MiB

// Transport defaults. Upstream registries (PyPI, npmjs.org, Maven Central) are
// HTTPS, so HTTP/2 is negotiated over TLS via ALPN (ForceAttemptHTTP2); these
// also bound connection reuse and the slow-upstream failure modes. Overall
// request deadlines are the caller's job via the request context, so there is
// deliberately no blanket http.Client.Timeout (it would also cap large artifact
// streams).
const (
	defaultDialTimeout           = 10 * time.Second
	defaultKeepAlive             = 30 * time.Second
	defaultTLSHandshakeTimeout   = 10 * time.Second
	defaultResponseHeaderTimeout = 30 * time.Second
	defaultIdleConnTimeout       = 90 * time.Second
	defaultExpectContinueTimeout = 1 * time.Second
	defaultMaxIdleConns          = 100
	// We mostly talk to a single upstream host, so the per-host idle pool is
	// raised well above net/http's default of 2 to keep connections warm.
	defaultMaxIdleConnsPerHost = 100
)

// newDefaultClient builds the upstream HTTP client used when none is injected:
// an HTTP/2-capable transport with a sane connection pool and timeouts.
func newDefaultClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ForceAttemptHTTP2:     true,
			DialContext:           (&net.Dialer{Timeout: defaultDialTimeout, KeepAlive: defaultKeepAlive}).DialContext,
			MaxIdleConns:          defaultMaxIdleConns,
			MaxIdleConnsPerHost:   defaultMaxIdleConnsPerHost,
			IdleConnTimeout:       defaultIdleConnTimeout,
			TLSHandshakeTimeout:   defaultTLSHandshakeTimeout,
			ExpectContinueTimeout: defaultExpectContinueTimeout,
			ResponseHeaderTimeout: defaultResponseHeaderTimeout,
		},
	}
}

// ErrOversized is returned when an upstream body exceeds the configured cap. It
// is returned before the full body is buffered.
var ErrOversized = errors.New("httpclient: upstream body exceeds max bytes")

// Client performs capped, context-aware GET/HEAD requests against an upstream
// registry. It is safe for concurrent use.
type Client struct {
	http         *http.Client
	maxBodyBytes int64
}

// Option customizes a Client at construction.
type Option func(*Client)

// WithHTTPClient injects the underlying *http.Client (tests use this to point
// at an httptest server or a roundtripper). A nil client is ignored.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h != nil {
			c.http = h
		}
	}
}

// WithMaxBodyBytes sets the buffered-body cap for GET. A non-positive value
// restores the default.
func WithMaxBodyBytes(n int64) Option {
	return func(c *Client) {
		if n > 0 {
			c.maxBodyBytes = n
		}
	}
}

// New constructs a Client. With no options it uses an HTTP/2-capable client with
// sane connection-pool and timeout defaults (see newDefaultClient) and
// DefaultMaxBodyBytes. WithHTTPClient overrides the client wholesale.
func New(opts ...Option) *Client {
	c := &Client{
		http:         newDefaultClient(),
		maxBodyBytes: DefaultMaxBodyBytes,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Response is an upstream reply. Body is nil for HEAD. The named header fields
// are pulled out for convenience; Header carries the full set for anything
// else.
type Response struct {
	Status       int
	Body         []byte
	ContentType  string
	ETag         string
	LastModified string
	CacheControl string
	Header       http.Header
}

// IsOK reports a 2xx status.
func (r *Response) IsOK() bool { return r.Status >= 200 && r.Status < 300 }

// IsNotFound reports a 404, distinguishing "upstream does not have this" from
// an unavailable or malformed upstream.
func (r *Response) IsNotFound() bool { return r.Status == http.StatusNotFound }

// IsServerError reports a 5xx status — the upstream is unavailable or broken.
func (r *Response) IsServerError() bool { return r.Status >= 500 }

// Get fetches url, buffering the body up to the configured cap. A body that
// would exceed the cap yields ErrOversized. HTTP status codes are not errors;
// inspect Response.Status (or IsOK/IsNotFound/IsServerError).
func (c *Client) Get(ctx context.Context, url string) (*Response, error) {
	resp, err := c.do(ctx, http.MethodGet, url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := c.readCapped(resp.Body)
	if err != nil {
		return nil, err
	}
	out := newResponse(resp)
	out.Body = body
	return out, nil
}

// StreamResponse is an upstream reply whose Body is left open and unread. Unlike
// Get, the body is not buffered or capped, so it suits large artifacts that
// should be streamed straight through to a client (and teed into storage). The
// caller MUST close Body. The overall transfer is bounded by the request
// context, not by maxBodyBytes.
type StreamResponse struct {
	Status        int
	Body          io.ReadCloser
	ContentType   string
	ContentLength int64
	Header        http.Header
}

// IsOK reports a 2xx status.
func (r *StreamResponse) IsOK() bool { return r.Status >= 200 && r.Status < 300 }

// IsNotFound reports a 404.
func (r *StreamResponse) IsNotFound() bool { return r.Status == http.StatusNotFound }

// IsServerError reports a 5xx status.
func (r *StreamResponse) IsServerError() bool { return r.Status >= 500 }

// Stream issues a GET and returns the response with its body open and unread for
// the caller to stream and close. HTTP status codes are not errors; inspect
// Status (or IsOK/IsNotFound/IsServerError) — note that even a non-2xx response
// carries an open Body the caller must close.
func (c *Client) Stream(ctx context.Context, url string) (*StreamResponse, error) {
	resp, err := c.do(ctx, http.MethodGet, url)
	if err != nil {
		return nil, err
	}
	return &StreamResponse{
		Status:        resp.StatusCode,
		Body:          resp.Body,
		ContentType:   resp.Header.Get("Content-Type"),
		ContentLength: resp.ContentLength,
		Header:        resp.Header,
	}, nil
}

// Head issues a HEAD request and returns headers and status without a body.
func (c *Client) Head(ctx context.Context, url string) (*Response, error) {
	resp, err := c.do(ctx, http.MethodHead, url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return newResponse(resp), nil
}

func (c *Client) do(ctx context.Context, method, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, fmt.Errorf("httpclient: build %s %q: %w", method, url, err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("httpclient: %s %q: %w", method, url, err)
	}
	return resp, nil
}

// readCapped reads up to maxBodyBytes from r, returning ErrOversized if the
// body is larger. It reads one extra byte to detect the overflow without
// buffering the whole oversized body.
func (c *Client) readCapped(r io.Reader) ([]byte, error) {
	limited := io.LimitReader(r, c.maxBodyBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("httpclient: read body: %w", err)
	}
	if int64(len(body)) > c.maxBodyBytes {
		return nil, fmt.Errorf("%w: cap %d bytes", ErrOversized, c.maxBodyBytes)
	}
	return body, nil
}

func newResponse(resp *http.Response) *Response {
	return &Response{
		Status:       resp.StatusCode,
		ContentType:  resp.Header.Get("Content-Type"),
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		CacheControl: resp.Header.Get("Cache-Control"),
		Header:       resp.Header,
	}
}
