package pypi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Upstream sentinel errors. The handler maps ErrUpstreamNotFound to a 404 and
// the rest to a 502 Bad Gateway.
var (
	// ErrUpstreamNotFound means the upstream reported the project or file does
	// not exist (HTTP 404).
	ErrUpstreamNotFound = errors.New("pypi upstream: not found")
	// ErrUpstreamUnavailable means the upstream could not be reached or
	// returned an unexpected status.
	ErrUpstreamUnavailable = errors.New("pypi upstream: unavailable")
	// ErrUpstreamMalformed means the upstream response could not be decoded
	// against the expected PEP 691 shape.
	ErrUpstreamMalformed = errors.New("pypi upstream: malformed response")
)

const defaultUserAgent = "open-artifact-pypi"

// UpstreamClient is the outbound PyPI client used for proxy / cache-through.
// It speaks the PEP 691 JSON simple API for index listings (so the handler
// re-renders rather than rewrites HTML) and streams file bytes straight from
// the URL the index advertised. A zero-value UpstreamClient is not usable;
// construct one with NewUpstreamClient.
type UpstreamClient struct {
	base *url.URL
	hc   *http.Client
	ua   string
}

// UpstreamOption customizes an UpstreamClient.
type UpstreamOption func(*UpstreamClient)

// WithHTTPClient overrides the HTTP client used for upstream requests. The
// default is a client with a 30s timeout.
func WithHTTPClient(hc *http.Client) UpstreamOption {
	return func(c *UpstreamClient) { c.hc = hc }
}

// WithUserAgent overrides the User-Agent sent upstream.
func WithUserAgent(ua string) UpstreamOption {
	return func(c *UpstreamClient) {
		if ua != "" {
			c.ua = ua
		}
	}
}

// NewUpstreamClient builds a client that talks to base, which must be an
// absolute http/https URL (the canonical PyPI value is "https://pypi.org").
func NewUpstreamClient(base string, opts ...UpstreamOption) (*UpstreamClient, error) {
	if base == "" {
		return nil, errors.New("pypi upstream: base URL is required")
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("pypi upstream: parse base %q: %w", base, err)
	}
	if !u.IsAbs() || u.Host == "" {
		return nil, fmt.Errorf("pypi upstream: base %q must be absolute", base)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("pypi upstream: base %q scheme must be http or https", base)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	c := &UpstreamClient{
		base: u,
		hc:   &http.Client{Timeout: 30 * time.Second},
		ua:   defaultUserAgent,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// Base returns the canonical upstream URL the client talks to.
func (c *UpstreamClient) Base() *url.URL { return c.base }

// UpstreamFile is a single distribution advertised by the upstream simple
// index, carrying the bytes' download URL and hashes.
type UpstreamFile struct {
	Filename string
	URL      string
	Hashes   map[string]string
}

// SHA256 returns the file's hex sha256 (no prefix), or "" when absent.
func (f UpstreamFile) SHA256() string { return f.Hashes["sha256"] }

// ProjectIndex is the upstream's view of one project.
type ProjectIndex struct {
	Name  string
	Files []UpstreamFile
}

// Find returns the file with the given name, or false when absent.
func (p *ProjectIndex) Find(filename string) (UpstreamFile, bool) {
	for _, f := range p.Files {
		if f.Filename == filename {
			return f, true
		}
	}
	return UpstreamFile{}, false
}

// pep691Project is the PEP 691 JSON shape for a per-project index.
type pep691Project struct {
	Name  string `json:"name"`
	Files []struct {
		Filename string            `json:"filename"`
		URL      string            `json:"url"`
		Hashes   map[string]string `json:"hashes"`
	} `json:"files"`
}

// pep691List is the PEP 691 JSON shape for the root project list.
type pep691List struct {
	Projects []struct {
		Name string `json:"name"`
	} `json:"projects"`
}

// Project fetches the per-project simple index for pkg (the PEP 503 normalized
// name) as PEP 691 JSON. Upstream-relative file URLs are resolved against the
// request URL so the handler always gets absolute download URLs.
func (c *UpstreamClient) Project(ctx context.Context, pkg string) (*ProjectIndex, error) {
	if pkg == "" {
		return nil, fmt.Errorf("pypi upstream: Project: package is required")
	}
	reqURL := c.base.JoinPath("simple", pkg)
	reqURL.Path += "/"
	body, err := c.getJSON(ctx, reqURL.String())
	if err != nil {
		return nil, err
	}
	var raw pep691Project
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("pypi upstream: decode project %q: %v: %w", pkg, err, ErrUpstreamMalformed)
	}
	out := &ProjectIndex{Name: raw.Name, Files: make([]UpstreamFile, 0, len(raw.Files))}
	if out.Name == "" {
		out.Name = pkg
	}
	for _, f := range raw.Files {
		abs := f.URL
		if u, perr := reqURL.Parse(f.URL); perr == nil {
			abs = u.String()
		}
		out.Files = append(out.Files, UpstreamFile{Filename: f.Filename, URL: abs, Hashes: f.Hashes})
	}
	return out, nil
}

// TopLevel fetches the root simple index as PEP 691 JSON, returning every
// project name the upstream advertises.
func (c *UpstreamClient) TopLevel(ctx context.Context) ([]string, error) {
	reqURL := c.base.JoinPath("simple")
	reqURL.Path += "/"
	body, err := c.getJSON(ctx, reqURL.String())
	if err != nil {
		return nil, err
	}
	var raw pep691List
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("pypi upstream: decode top-level index: %v: %w", err, ErrUpstreamMalformed)
	}
	names := make([]string, 0, len(raw.Projects))
	for _, p := range raw.Projects {
		names = append(names, p.Name)
	}
	return names, nil
}

// FileResponse is a streaming file body fetched from the upstream. The caller
// must close Body.
type FileResponse struct {
	Body          io.ReadCloser
	ContentType   string
	ContentLength int64
}

// FetchFile opens a streaming GET against rawURL, expected to have come from a
// prior Project response so it is already validated against the upstream
// surface. The returned Body must be closed by the caller.
func (c *UpstreamClient) FetchFile(ctx context.Context, rawURL string) (*FileResponse, error) {
	if rawURL == "" {
		return nil, fmt.Errorf("pypi upstream: FetchFile: url is required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("pypi upstream: new request %q: %w", rawURL, err)
	}
	req.Header.Set("User-Agent", c.ua)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pypi upstream: fetch %q: %v: %w", rawURL, err, ErrUpstreamUnavailable)
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, fmt.Errorf("pypi upstream: file %q: %w", rawURL, ErrUpstreamNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("pypi upstream: file %q: status %d: %w", rawURL, resp.StatusCode, ErrUpstreamUnavailable)
	}
	return &FileResponse{
		Body:          resp.Body,
		ContentType:   resp.Header.Get("Content-Type"),
		ContentLength: resp.ContentLength,
	}, nil
}

// getJSON performs a GET requesting the PEP 691 JSON representation and returns
// the body, mapping status codes to the upstream sentinels.
func (c *UpstreamClient) getJSON(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("pypi upstream: new request %q: %w", rawURL, err)
	}
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("Accept", contentTypeJSONv1+", application/json;q=0.9")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pypi upstream: get %q: %v: %w", rawURL, err, ErrUpstreamUnavailable)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("pypi upstream: get %q: %w", rawURL, ErrUpstreamNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pypi upstream: get %q: status %d: %w", rawURL, resp.StatusCode, ErrUpstreamUnavailable)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("pypi upstream: read %q: %v: %w", rawURL, err, ErrUpstreamUnavailable)
	}
	return body, nil
}
