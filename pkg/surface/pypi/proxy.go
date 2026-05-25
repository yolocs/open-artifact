package pypi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"

	"github.com/yolocs/open-artifact/pkg/core"
	"github.com/yolocs/open-artifact/pkg/logging"
	"github.com/yolocs/open-artifact/pkg/namespace"
	"github.com/yolocs/open-artifact/pkg/observability"
	"github.com/yolocs/open-artifact/pkg/proxy/filter"
	"github.com/yolocs/open-artifact/pkg/proxy/httpclient"
	"github.com/yolocs/open-artifact/pkg/proxy/negcache"
	"github.com/yolocs/open-artifact/pkg/proxy/singleflight"
	"github.com/yolocs/open-artifact/pkg/surface"
)

// errUpstreamUnavailable means an upstream index refresh failed and no stale or
// synthesized fallback was available. It maps to 503.
var errUpstreamUnavailable = errors.New("pypi: upstream unavailable")

// proxyEngine holds the pull-through machinery for proxy-mode PyPI namespaces.
//
// Index/metadata caching is two-level. An in-process memo (mem) absorbs bursty
// requests for a short TTL. Behind it, the namespace's blob Store holds a
// durable snapshot of the last good upstream index (a .cache/ entry). Crucially,
// while upstream is reachable the durable snapshot is write-through only — every
// miss past the memo refetches upstream and overwrites the snapshot; the
// snapshot is *read* only as a fallback when upstream is unavailable. Artifact
// bytes are handled differently: they are cached on first pull as real Files and
// served from our storage thereafter (see download).
type proxyEngine struct {
	now     func() time.Time
	httpc   *httpclient.Client
	neg     *negcache.Cache
	mem     *indexMemo
	sfIndex singleflight.Group[*proxyIndex]
}

func newProxyEngine(cfg Config, now func() time.Time) *proxyEngine {
	return &proxyEngine{
		now:   now,
		httpc: httpclient.New(),
		neg:   negcache.New(cfg.ProxyNegativeCacheTTL),
		mem:   newIndexMemo(cfg.proxyIndexCacheTTL(), now),
	}
}

// indexMemo is the in-process, short-TTL cache of parsed upstream indexes. It is
// the burst-absorbing first level in front of upstream; it never holds negative
// results (those live in the negative cache) and is not durable.
type indexMemo struct {
	ttl time.Duration
	now func() time.Time
	mu  sync.Mutex
	m   map[string]indexMemoEntry
}

type indexMemoEntry struct {
	idx     *proxyIndex
	expires time.Time
}

func newIndexMemo(ttl time.Duration, now func() time.Time) *indexMemo {
	return &indexMemo{ttl: ttl, now: now, m: make(map[string]indexMemoEntry)}
}

func (c *indexMemo) get(key string) (*proxyIndex, bool) {
	if c.ttl <= 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.m[key]; ok && c.now().Before(e.expires) {
		return e.idx, true
	}
	return nil, false
}

func (c *indexMemo) put(key string, idx *proxyIndex) {
	if c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	c.m[key] = indexMemoEntry{idx: idx, expires: c.now().Add(c.ttl)}
	c.mu.Unlock()
}

// proxyIndex is the canonical metadata cached for a project's upstream simple
// index. It carries enough to render a local simple index and to resolve the
// upstream URL for each file on a download cold fill. It is the JSON body stored
// in the .cache/ metadata entry keyed "pypi:simple:<project>".
type proxyIndex struct {
	Name  string          `json:"name"`
	Files []proxyFileMeta `json:"files"`
}

type proxyFileMeta struct {
	Filename       string     `json:"filename"`
	UpstreamURL    string     `json:"upstream_url,omitempty"`
	SHA256         string     `json:"sha256,omitempty"`
	RequiresPython string     `json:"requires_python,omitempty"`
	Version        string     `json:"version,omitempty"`
	UploadTime     *time.Time `json:"upload_time,omitempty"`
}

// find returns the metadata for filename, requiring the version to match when
// it was discoverable. A filename is unique within a project (it encodes the
// version), so the filename is the primary key.
func (idx *proxyIndex) find(filename, version string) (proxyFileMeta, bool) {
	for _, f := range idx.Files {
		if f.Filename != filename {
			continue
		}
		if f.Version != "" && f.Version != version {
			return proxyFileMeta{}, false
		}
		return f, true
	}
	return proxyFileMeta{}, false
}

func (e *proxyEngine) projectIndex(w http.ResponseWriter, r *http.Request, ns string, spec namespace.Spec, store core.Store, rawProject string) {
	project, err := NormalizeProject(rawProject)
	if err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	idx, err := e.fetchIndex(r.Context(), ns, store, spec, project)
	if err != nil {
		e.writeIndexError(w, r, err)
		return
	}
	page := e.renderPage(ns, idx)
	if PrefersSimpleJSON(r.Header.Get("Accept")) {
		w.Header().Set("Content-Type", simpleJSONMediaType)
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodHead {
			return
		}
		_ = json.NewEncoder(w).Encode(ProjectJSON(page))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.WriteString(w, RenderProjectHTML(page))
}

func (e *proxyEngine) download(w http.ResponseWriter, r *http.Request, ns string, spec namespace.Spec, store core.Store, rawProject, version, filename string) {
	project, err := NormalizeProject(rawProject)
	if err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	if err := ValidateVersion(version); err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	if err := ValidateFilename(filename); err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}

	// Serve from the local cache when the file has already been pulled through.
	local := store.Package(project).Version(version).File(filename)
	exists, err := local.Exists(r.Context())
	if err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	if exists {
		surface.RedirectOrStreamFile(w, r, local, "application/octet-stream")
		return
	}

	if e.neg.Has(ns, pypiFormat, fileKey(project, version, filename)) {
		surface.WriteStoreError(w, r, core.ErrNotFound)
		return
	}

	idx, err := e.fetchIndex(r.Context(), ns, store, spec, project)
	if err != nil {
		e.writeIndexError(w, r, err)
		return
	}
	meta, ok := idx.find(filename, version)
	if !ok || meta.UpstreamURL == "" {
		// No upstream source for this file: either upstream does not list it,
		// or it is a synthesized-from-local entry whose bytes we already proved
		// absent above.
		e.neg.Mark(ns, pypiFormat, fileKey(project, version, filename))
		surface.WriteStoreError(w, r, core.ErrNotFound)
		return
	}

	if !e.allow(r, project, version, filename, spec, meta) {
		// Denied artifacts look like 404 to package clients; the reason is logged.
		surface.WriteStoreError(w, r, core.ErrNotFound)
		return
	}

	if r.Method == http.MethodHead {
		// Existence is answered from index metadata; bytes are not fetched or
		// cached on HEAD.
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		return
	}

	sr, err := e.httpc.Stream(r.Context(), meta.UpstreamURL)
	if err != nil {
		logging.FromContext(r.Context()).Error("proxy artifact fetch failed",
			logging.KeyComponent, "pypi", logging.KeyError, err)
		surface.WriteError(w, http.StatusBadGateway, "upstream fetch failed")
		return
	}
	defer sr.Body.Close()
	switch {
	case sr.IsNotFound():
		e.neg.Mark(ns, pypiFormat, fileKey(project, version, filename))
		surface.WriteStoreError(w, r, core.ErrNotFound)
		return
	case !sr.IsOK():
		logging.FromContext(r.Context()).Error("proxy artifact upstream status",
			logging.KeyComponent, "pypi", "status", sr.Status)
		surface.WriteError(w, http.StatusBadGateway, "upstream unavailable")
		return
	}

	e.streamAndCache(w, r, store, project, version, filename, meta, sr)
}

// streamAndCache streams the upstream artifact straight to the client while
// teeing the same bytes into the Store as a real File — no full-artifact
// buffering (see surface.TeeStreamToStore). The client is the primary consumer:
// a Store write failure is swallowed so it cannot truncate the client (the
// response is governed by upstream stream success). Conversely, if the client
// disconnects, the Store write aborts cleanly (blobstore cancels the partial
// write), so no truncated File is left to poison the cache. We do not verify the
// index-advertised sha256 here — it shares the upstream's trust — but record it
// for clients to verify end to end; the local File's own digest is authoritative.
func (e *proxyEngine) streamAndCache(w http.ResponseWriter, r *http.Request, store core.Store, project, version, filename string, meta proxyFileMeta, sr *httpclient.StreamResponse) {
	ctx := r.Context()

	copyErr, fillErr := surface.TeeStreamToStore(w, sr.Body, "application/octet-stream", sr.ContentLength, func(src io.Reader) error {
		ann := proxyFileAnnotations(project, version, filename, meta, e.now().UTC())
		_, err := store.Package(project).Version(version).AddFile(ctx, filename, src, core.WithAnnotations(ann))
		return err
	})
	switch {
	case copyErr != nil:
		// The client read failed mid-stream (disconnect) or upstream ended early.
		// The Store write aborts with it; nothing servable is committed.
		logging.FromContext(ctx).Warn("proxy artifact stream interrupted",
			logging.KeyComponent, "pypi", "package", project, "version", version,
			"filename", filename, logging.KeyError, copyErr)
	case fillErr != nil:
		// Upstream fully delivered to the client, but caching failed. Log and
		// move on — the next request will refill.
		logging.FromContext(ctx).Warn("proxy cache fill failed",
			logging.KeyComponent, "pypi", "package", project, "version", version,
			"filename", filename, logging.KeyError, fillErr)
	default:
		// Cached successfully; record the package/version envelopes for parity
		// with hosted uploads (best-effort, metadata only).
		e.recordParents(ctx, store, project, version)
	}
}

// recordParents writes the package and version metadata envelopes after a
// successful fill, mirroring the hosted upload annotations. Best-effort.
func (e *proxyEngine) recordParents(ctx context.Context, store core.Store, project, version string) {
	if _, err := store.AddPackage(ctx, project, core.WithAnnotations(map[string]any{"pypi:name": project})); err != nil && !errors.Is(err, core.ErrAlreadyExists) {
		logging.FromContext(ctx).Warn("proxy cache fill add package failed",
			logging.KeyComponent, "pypi", logging.KeyError, err)
		return
	}
	if _, err := store.Package(project).AddVersion(ctx, version, core.WithAnnotations(map[string]any{"pypi:version": version})); err != nil && !errors.Is(err, core.ErrAlreadyExists) {
		logging.FromContext(ctx).Warn("proxy cache fill add version failed",
			logging.KeyComponent, "pypi", logging.KeyError, err)
	}
}

// fetchIndex returns the upstream index for project through the two-level cache:
// the in-process memo (burst absorber) first, then a singleflight-coalesced
// load that goes to upstream and falls back to the durable snapshot only when
// upstream is unavailable. A remembered upstream 404 short-circuits to NotFound.
func (e *proxyEngine) fetchIndex(ctx context.Context, ns string, store core.Store, spec namespace.Spec, project string) (*proxyIndex, error) {
	key := ns + "\x00" + project
	if idx, ok := e.mem.get(key); ok {
		return idx, nil
	}
	if e.neg.Has(ns, pypiFormat, simpleKey(project)) {
		return nil, core.ErrNotFound
	}
	idx, err, _ := e.sfIndex.Do(key, func() (*proxyIndex, error) {
		if idx, ok := e.mem.get(key); ok {
			return idx, nil
		}
		return e.loadIndex(ctx, ns, store, spec, project)
	})
	if err != nil {
		return nil, err
	}
	e.mem.put(key, idx)
	return idx, nil
}

// loadIndex fetches the upstream index and, on success, overwrites the durable
// snapshot (write-through). The snapshot is read only when upstream is
// unavailable — see fallbackIndex. A clean upstream 404 is negative-cached.
func (e *proxyEngine) loadIndex(ctx context.Context, ns string, store core.Store, spec namespace.Spec, project string) (*proxyIndex, error) {
	key := simpleKey(project)
	resp, err := e.httpc.Get(ctx, e.simpleURL(spec, project))
	if err != nil {
		logging.FromContext(ctx).Warn("proxy index fetch failed",
			logging.KeyComponent, "pypi", logging.KeyError, err)
		return e.fallbackIndex(ctx, store, project)
	}
	switch {
	case resp.IsNotFound():
		e.neg.Mark(ns, pypiFormat, key)
		return nil, core.ErrNotFound
	case resp.IsOK():
		idx, perr := parseUpstreamIndex(resp, e.simpleURL(spec, project), project)
		if perr != nil {
			logging.FromContext(ctx).Warn("proxy index parse failed",
				logging.KeyComponent, "pypi", logging.KeyError, perr)
			return e.fallbackIndex(ctx, store, project)
		}
		if b, err := json.Marshal(idx); err == nil {
			if _, err := store.AddCache(ctx, key, bytes.NewReader(b)); err != nil {
				logging.FromContext(ctx).Warn("proxy index cache write failed",
					logging.KeyComponent, "pypi", logging.KeyError, err)
			}
		}
		e.neg.Delete(ns, pypiFormat, key)
		return idx, nil
	default:
		logging.FromContext(ctx).Warn("proxy index upstream status",
			logging.KeyComponent, "pypi", "status", resp.Status)
		return e.fallbackIndex(ctx, store, project)
	}
}

// fallbackIndex is the upstream-unavailable path: serve the durable snapshot at
// any age, else a minimal index synthesized from locally cached files, else
// report the upstream as unavailable (503).
func (e *proxyEngine) fallbackIndex(ctx context.Context, store core.Store, project string) (*proxyIndex, error) {
	cf := store.Cache(simpleKey(project))
	if exists, err := cf.Exists(ctx); err == nil && exists {
		if idx, err := readCachedIndex(ctx, cf); err == nil {
			return idx, nil
		}
	}
	if syn, ok := e.synthesize(ctx, store, project); ok {
		return syn, nil
	}
	return nil, errUpstreamUnavailable
}

// synthesize builds a minimal index from the files already cached in the Store,
// used when upstream is unavailable and no cached index exists.
func (e *proxyEngine) synthesize(ctx context.Context, store core.Store, project string) (*proxyIndex, bool) {
	pkg := store.Package(project)
	if exists, err := pkg.Exists(ctx); err != nil || !exists {
		return nil, false
	}
	versions, err := pkg.Versions(ctx)
	if err != nil {
		return nil, false
	}
	idx := &proxyIndex{Name: project}
	for _, v := range versions {
		files, err := v.Files(ctx)
		if err != nil {
			return nil, false
		}
		for _, f := range files {
			meta, err := f.Meta(ctx)
			if err != nil {
				continue
			}
			idx.Files = append(idx.Files, proxyFileMeta{
				Filename:       f.Name(),
				Version:        v.Name(),
				SHA256:         HashFromDigest(meta.Digest),
				RequiresPython: annotationString(meta.Annotations, "pypi:requires_python"),
			})
		}
	}
	if len(idx.Files) == 0 {
		return nil, false
	}
	return idx, true
}

func (e *proxyEngine) renderPage(ns string, idx *proxyIndex) ProjectPage {
	page := ProjectPage{Name: idx.Name}
	for _, f := range idx.Files {
		if f.Version == "" {
			// Without a version we cannot route a local download URL.
			continue
		}
		page.Files = append(page.Files, FileLink{
			Filename:       f.Filename,
			URL:            packageURL(ns, idx.Name, f.Version, f.Filename),
			SHA256:         f.SHA256,
			RequiresPython: f.RequiresPython,
		})
	}
	return page
}

// allow evaluates the namespace filter chain for an artifact download. A delay
// filter that needs a publish time triggers an upstream JSON metadata lookup;
// if the time is still unknown the decision fails closed (denied).
func (e *proxyEngine) allow(r *http.Request, project, version, filename string, spec namespace.Spec, meta proxyFileMeta) bool {
	if len(spec.Proxy.Filters) == 0 {
		return true
	}
	chain, err := filter.Compile(spec.Proxy.Filters)
	if err != nil {
		logging.FromContext(r.Context()).Error("proxy filter compile failed",
			logging.KeyComponent, "pypi", logging.KeyError, err)
		return false
	}
	ref := filter.Ref{Package: project, Version: version, PublishedAt: meta.UploadTime}
	dec := chain.DecideAt(ref, e.now())
	if dec.NeedsMetadata() {
		if t := e.resolveUploadTime(r.Context(), spec, project, version, filename); t != nil {
			ref.PublishedAt = t
			dec = chain.DecideAt(ref, e.now())
		}
	}
	if dec.Allowed() {
		return true
	}
	logging.FromContext(r.Context()).Info("proxy artifact denied by filter",
		logging.KeyComponent, "pypi", "package", project, "version", version,
		"filename", filename, "reason", dec.Reason)
	return false
}

// resolveUploadTime fetches the upstream per-release JSON metadata to learn a
// file's publish time when a delay filter needs it. A failure returns nil and
// the caller fails closed.
func (e *proxyEngine) resolveUploadTime(ctx context.Context, spec namespace.Spec, project, version, filename string) *time.Time {
	u := upstreamBase(spec) + "/pypi/" + url.PathEscape(project) + "/" + url.PathEscape(version) + "/json"
	resp, err := e.httpc.Get(ctx, u)
	if err != nil || !resp.IsOK() {
		return nil
	}
	var doc struct {
		URLs []struct {
			Filename   string `json:"filename"`
			UploadTime string `json:"upload_time_iso_8601"`
		} `json:"urls"`
	}
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		return nil
	}
	for _, f := range doc.URLs {
		if f.Filename == filename && f.UploadTime != "" {
			if t, err := time.Parse(time.RFC3339, f.UploadTime); err == nil {
				return &t
			}
		}
	}
	return nil
}

func (e *proxyEngine) writeIndexError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, core.ErrNotFound):
		surface.WriteStoreError(w, r, core.ErrNotFound)
	case errors.Is(err, errUpstreamUnavailable):
		observability.RecordError(r, err)
		surface.WriteError(w, http.StatusServiceUnavailable, "upstream unavailable")
	default:
		surface.WriteStoreError(w, r, err)
	}
}

func (e *proxyEngine) simpleURL(spec namespace.Spec, project string) string {
	return upstreamBase(spec) + "/simple/" + url.PathEscape(project) + "/"
}

func upstreamBase(spec namespace.Spec) string {
	return strings.TrimRight(spec.Proxy.Upstream, "/")
}

func simpleKey(project string) string { return "pypi:simple:" + project }

func fileKey(project, version, filename string) string {
	return "pypi:file:" + project + ":" + version + ":" + filename
}

func readCachedIndex(ctx context.Context, cf core.CacheFile) (*proxyIndex, error) {
	rc, err := cf.Read(ctx)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	var idx proxyIndex
	if err := json.Unmarshal(b, &idx); err != nil {
		return nil, err
	}
	return &idx, nil
}

func proxyFileAnnotations(project, version, filename string, meta proxyFileMeta, fetchedAt time.Time) map[string]any {
	out := map[string]any{
		"pypi:name":     project,
		"pypi:version":  version,
		"pypi:filename": filename,
	}
	if meta.UpstreamURL != "" {
		out["pypi:upstream_url"] = meta.UpstreamURL
	}
	if meta.SHA256 != "" {
		out["pypi:sha256_digest"] = meta.SHA256
	}
	if meta.RequiresPython != "" {
		out["pypi:requires_python"] = meta.RequiresPython
	}
	if meta.UploadTime != nil {
		out["pypi:uploaded_at"] = meta.UploadTime.UTC().Format(time.RFC3339Nano)
	} else {
		out["pypi:uploaded_at"] = fetchedAt.Format(time.RFC3339Nano)
	}
	return out
}

// parseUpstreamIndex parses an upstream simple index, choosing PEP 691 JSON or
// PEP 503 HTML by the response content type. base is the simple index URL used
// to resolve relative file links.
func parseUpstreamIndex(resp *httpclient.Response, base, project string) (*proxyIndex, error) {
	if strings.Contains(strings.ToLower(resp.ContentType), "json") {
		return parseJSONIndex(resp.Body, base, project)
	}
	return parseHTMLIndex(resp.Body, base, project)
}

func parseJSONIndex(body []byte, base, project string) (*proxyIndex, error) {
	var doc struct {
		Files []struct {
			Filename       string            `json:"filename"`
			URL            string            `json:"url"`
			Hashes         map[string]string `json:"hashes"`
			RequiresPython string            `json:"requires-python"`
			UploadTime     string            `json:"upload-time"`
		} `json:"files"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	idx := &proxyIndex{Name: project}
	for _, f := range doc.Files {
		if ValidateFilename(f.Filename) != nil {
			continue
		}
		abs := resolveURL(base, f.URL)
		sha := f.Hashes["sha256"]
		if sha == "" {
			sha = shaFromFragment(abs)
		}
		var pub *time.Time
		if f.UploadTime != "" {
			if t, err := time.Parse(time.RFC3339, f.UploadTime); err == nil {
				pub = &t
			}
		}
		idx.Files = append(idx.Files, proxyFileMeta{
			Filename:       f.Filename,
			UpstreamURL:    stripFragment(abs),
			SHA256:         sha,
			RequiresPython: f.RequiresPython,
			Version:        deriveVersion(f.Filename),
			UploadTime:     pub,
		})
	}
	return idx, nil
}

func parseHTMLIndex(body []byte, base, project string) (*proxyIndex, error) {
	root, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	idx := &proxyIndex{Name: project}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			var href, requires string
			for _, a := range n.Attr {
				switch a.Key {
				case "href":
					href = a.Val
				case "data-requires-python":
					requires = a.Val
				}
			}
			filename := strings.TrimSpace(nodeText(n))
			if href != "" && ValidateFilename(filename) == nil {
				abs := resolveURL(base, href)
				idx.Files = append(idx.Files, proxyFileMeta{
					Filename:       filename,
					UpstreamURL:    stripFragment(abs),
					SHA256:         shaFromFragment(abs),
					RequiresPython: requires,
					Version:        deriveVersion(filename),
				})
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return idx, nil
}

func nodeText(n *html.Node) string {
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
		}
	}
	return b.String()
}

// deriveVersion extracts the release version from a wheel or sdist filename. It
// returns "" when the version cannot be derived safely.
func deriveVersion(filename string) string {
	switch {
	case strings.HasSuffix(filename, ".whl"):
		parts := strings.Split(strings.TrimSuffix(filename, ".whl"), "-")
		if len(parts) >= 2 {
			return parts[1]
		}
	case strings.HasSuffix(filename, ".tar.gz"), strings.HasSuffix(filename, ".zip"):
		base := strings.TrimSuffix(strings.TrimSuffix(filename, ".tar.gz"), ".zip")
		if i := strings.LastIndex(base, "-"); i >= 0 && i+1 < len(base) {
			return base[i+1:]
		}
	}
	return ""
}

func resolveURL(base, href string) string {
	b, err := url.Parse(base)
	if err != nil {
		return href
	}
	u, err := url.Parse(href)
	if err != nil {
		return href
	}
	return b.ResolveReference(u).String()
}

func stripFragment(raw string) string {
	if i := strings.IndexByte(raw, '#'); i >= 0 {
		return raw[:i]
	}
	return raw
}

func shaFromFragment(raw string) string {
	i := strings.IndexByte(raw, '#')
	if i < 0 {
		return ""
	}
	frag := raw[i+1:]
	for _, part := range strings.Split(frag, "&") {
		if v, ok := strings.CutPrefix(part, "sha256="); ok {
			return v
		}
	}
	return ""
}
