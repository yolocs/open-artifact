package npm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

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

// errUpstreamUnavailable means an upstream packument refresh failed and no
// stale durable snapshot or synthesized fallback was available. It maps to 503.
var errUpstreamUnavailable = errors.New("npm: upstream unavailable")

// proxyEngine holds the pull-through machinery for proxy-mode npm namespaces.
//
// Packument caching is two-level. An in-process memo (memo) absorbs bursty
// requests for a short TTL. Behind it, the namespace's blob Store holds a
// durable snapshot of the last good upstream packument (a .cache/ entry keyed
// npm:packument:<core>). Unlike the PyPI proxy — whose durable snapshot is
// write-through and read only when upstream is unavailable — the npm durable
// snapshot is served directly while it is fresh (younger than durTTL, ~10m),
// without contacting upstream. npm packuments are heavier and change far less
// often than per-request, so a short durable TTL bounds staleness while cutting
// upstream load; the burst memo still absorbs the within-TTL stampede. When
// upstream is unreachable the durable snapshot is served at any age, then a
// minimal packument synthesized from locally cached tarballs, else 503.
//
// Artifact (tarball) bytes are handled differently: pulled on first request as
// real Files and served from our storage thereafter.
type proxyEngine struct {
	now    func() time.Time
	httpc  *httpclient.Client
	neg    *negcache.Cache
	memo   *packumentMemo
	durTTL time.Duration
	sf     singleflight.Group[*proxyPackument]
}

func newProxyEngine(cfg Config, now func() time.Time) *proxyEngine {
	return &proxyEngine{
		now:    now,
		httpc:  httpclient.New(),
		neg:    negcache.New(cfg.ProxyNegativeCacheTTL),
		memo:   newPackumentMemo(cfg.proxyPackumentMemoTTL(), now),
		durTTL: cfg.proxyPackumentCacheTTL(),
	}
}

// packumentMemo is the in-process, short-TTL cache of parsed upstream
// packuments. It is the burst-absorbing first level in front of the durable
// snapshot and upstream; it never holds negative results (those live in the
// negative cache) and is not durable. A non-positive TTL disables it.
type packumentMemo struct {
	ttl time.Duration
	now func() time.Time
	mu  sync.Mutex
	m   map[string]packumentMemoEntry
}

type packumentMemoEntry struct {
	pkmt    *proxyPackument
	expires time.Time
}

func newPackumentMemo(ttl time.Duration, now func() time.Time) *packumentMemo {
	return &packumentMemo{ttl: ttl, now: now, m: make(map[string]packumentMemoEntry)}
}

func (c *packumentMemo) get(key string) (*proxyPackument, bool) {
	if c.ttl <= 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.m[key]; ok && c.now().Before(e.expires) {
		return e.pkmt, true
	}
	return nil, false
}

func (c *packumentMemo) put(key string, pkmt *proxyPackument) {
	if c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	c.m[key] = packumentMemoEntry{pkmt: pkmt, expires: c.now().Add(c.ttl)}
	c.mu.Unlock()
}

// proxyPackument is the canonical metadata cached for a package's upstream
// packument. It is the JSON body stored in the .cache/ entry keyed
// npm:packument:<core>. Versions hold the full upstream version documents with
// dist.tarball left pointing at the upstream URL — tarball cold-fill reads that
// to find the source, and the serve path rewrites it to an open-artifact URL on
// the way out (so a cached upstream URL is never served to a client).
type proxyPackument struct {
	Name     string                    `json:"name"`
	DistTags map[string]string         `json:"dist-tags,omitempty"`
	Versions map[string]map[string]any `json:"versions"`
	Time     map[string]string         `json:"time,omitempty"`
}

// findByFilename returns the version and upstream tarball URL whose dist.tarball
// basename matches filename. A filename is unique within a package, so it is the
// primary key.
func (p *proxyPackument) findByFilename(filename string) (version, upstreamURL string, ok bool) {
	for v, doc := range p.Versions {
		if t := distTarball(doc); t != "" && path.Base(t) == filename {
			return v, t, true
		}
	}
	return "", "", false
}

// publishTime returns the parsed RFC3339 publish time for version from the
// packument's time map, or nil when it is absent or unparsable.
func (p *proxyPackument) publishTime(version string) *time.Time {
	if p.Time == nil {
		return nil
	}
	s, ok := p.Time[version]
	if !ok || s == "" {
		return nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return &t
	}
	return nil
}

func distTarball(doc map[string]any) string {
	dist, _ := doc["dist"].(map[string]any)
	if dist == nil {
		return ""
	}
	t, _ := dist["tarball"].(string)
	return t
}

// packument serves a proxy-mode packument read: rewrites every version's
// dist.tarball to this registry and emits the assembled document.
func (e *proxyEngine) packument(w http.ResponseWriter, r *http.Request, ns string, spec namespace.Spec, store core.Store, pn PackageName) {
	pkmt, err := e.fetchPackument(r.Context(), ns, store, spec, pn)
	if err != nil {
		e.writeError(w, r, err)
		return
	}
	doc := renderPackument(pkmt, requestBaseURL(r), ns, pn)
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		return
	}
	surface.WriteJSON(w, http.StatusOK, doc)
}

// distTags serves a proxy-mode dist-tags listing from the (cached) upstream
// packument. Local dist-tag mutation is rejected separately with 405.
func (e *proxyEngine) distTags(w http.ResponseWriter, r *http.Request, ns string, spec namespace.Spec, store core.Store, pn PackageName) {
	pkmt, err := e.fetchPackument(r.Context(), ns, store, spec, pn)
	if err != nil {
		e.writeError(w, r, err)
		return
	}
	tags := pkmt.DistTags
	if tags == nil {
		tags = map[string]string{}
	}
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		return
	}
	surface.WriteJSON(w, http.StatusOK, tags)
}

// tarball serves a proxy-mode tarball download: local cache hit, else resolve
// the version and upstream URL from the packument, evaluate the filter chain,
// and stream-with-tee from upstream into the Store.
func (e *proxyEngine) tarball(w http.ResponseWriter, r *http.Request, ns string, spec namespace.Spec, store core.Store, pn PackageName, filename string) {
	coreName := pn.Core()

	// Warm-cache fast path: the npm tarball filename encodes the version, so a
	// previously cached file can be served without fetching the packument.
	if version := deriveVersion(pn, filename); version != "" {
		local := store.Package(coreName).Version(version).File(filename)
		if exists, err := local.Exists(r.Context()); err != nil {
			surface.WriteStoreError(w, r, err)
			return
		} else if exists {
			surface.RedirectOrStreamFile(w, r, local, "application/octet-stream")
			return
		}
	}

	if e.neg.Has(ns, npmFormat, tarballKey(coreName, filename)) {
		surface.WriteStoreError(w, r, core.ErrNotFound)
		return
	}

	pkmt, err := e.fetchPackument(r.Context(), ns, store, spec, pn)
	if err != nil {
		e.writeError(w, r, err)
		return
	}

	version, upstreamURL, ok := pkmt.findByFilename(filename)
	if !ok || upstreamURL == "" {
		e.neg.Mark(ns, npmFormat, tarballKey(coreName, filename))
		surface.WriteStoreError(w, r, core.ErrNotFound)
		return
	}

	// Re-check the local Store now that the version is known (covers an unusual
	// filename the warm-path derivation missed).
	local := store.Package(coreName).Version(version).File(filename)
	if exists, err := local.Exists(r.Context()); err != nil {
		surface.WriteStoreError(w, r, err)
		return
	} else if exists {
		surface.RedirectOrStreamFile(w, r, local, "application/octet-stream")
		return
	}

	if !e.allow(r, pn, version, filename, spec, pkmt) {
		// Denied artifacts look like 404 to npm; the reason is logged.
		surface.WriteStoreError(w, r, core.ErrNotFound)
		return
	}

	if r.Method == http.MethodHead {
		// Existence is answered from packument metadata; bytes are not fetched or
		// cached on HEAD.
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		return
	}

	sr, err := e.httpc.Stream(r.Context(), upstreamURL)
	if err != nil {
		logging.FromContext(r.Context()).Error("proxy tarball fetch failed",
			logging.KeyComponent, "npm", logging.KeyError, err)
		surface.WriteError(w, http.StatusBadGateway, "upstream fetch failed")
		return
	}
	defer sr.Body.Close()
	switch {
	case sr.IsNotFound():
		e.neg.Mark(ns, npmFormat, tarballKey(coreName, filename))
		surface.WriteStoreError(w, r, core.ErrNotFound)
		return
	case !sr.IsOK():
		logging.FromContext(r.Context()).Error("proxy tarball upstream status",
			logging.KeyComponent, "npm", "status", sr.Status)
		surface.WriteError(w, http.StatusBadGateway, "upstream unavailable")
		return
	}

	e.streamAndCache(w, r, ns, store, pn, version, filename, upstreamURL, pkmt, sr)
}

// streamAndCache streams the upstream tarball straight to the client while
// teeing the same bytes into the Store as a real File (see
// surface.TeeStreamToStore). On a clean fill it records the package/version
// envelopes and the per-version package.json so the cache survives a process
// restart and can synthesize a packument when upstream is later unavailable.
func (e *proxyEngine) streamAndCache(w http.ResponseWriter, r *http.Request, ns string, store core.Store, pn PackageName, version, filename, upstreamURL string, pkmt *proxyPackument, sr *httpclient.StreamResponse) {
	ctx := r.Context()
	coreName := pn.Core()

	uploadedAt := e.now().UTC().Format(time.RFC3339Nano)
	if t := pkmt.publishTime(version); t != nil {
		uploadedAt = t.UTC().Format(time.RFC3339Nano)
	}
	ann := map[string]any{
		"npm:name":         pn.Original,
		"npm:version":      version,
		"npm:filename":     filename,
		"npm:upstream_url": upstreamURL,
		"npm:uploaded_at":  uploadedAt,
	}

	copyErr, fillErr := surface.TeeStreamToStore(w, sr.Body, "application/octet-stream", sr.ContentLength, func(src io.Reader) error {
		_, err := store.Package(coreName).Version(version).AddFile(ctx, filename, src, core.WithAnnotations(ann))
		return err
	})
	switch {
	case copyErr != nil:
		logging.FromContext(ctx).Warn("proxy tarball stream interrupted",
			logging.KeyComponent, "npm", "package", pn.Original, "version", version,
			"filename", filename, logging.KeyError, copyErr)
	case fillErr != nil:
		logging.FromContext(ctx).Warn("proxy cache fill failed",
			logging.KeyComponent, "npm", "package", pn.Original, "version", version,
			"filename", filename, logging.KeyError, fillErr)
	default:
		e.recordParents(ctx, requestBaseURL(r), ns, store, pn, version, uploadedAt, pkmt)
	}
}

// recordParents writes the package/version envelopes and the per-version
// package.json after a successful fill, mirroring the hosted upload layout. The
// stored package.json has dist.tarball rewritten back to open-artifact.
// Best-effort: a failure is logged and the next request refills.
func (e *proxyEngine) recordParents(ctx context.Context, base, ns string, store core.Store, pn PackageName, version, uploadedAt string, pkmt *proxyPackument) {
	coreName := pn.Core()
	if _, err := store.AddPackage(ctx, coreName, core.WithAnnotations(map[string]any{"npm:name": pn.Original})); err != nil && !errors.Is(err, core.ErrAlreadyExists) {
		logging.FromContext(ctx).Warn("proxy cache fill add package failed",
			logging.KeyComponent, "npm", logging.KeyError, err)
		return
	}
	if _, err := store.Package(coreName).AddVersion(ctx, version, core.WithAnnotations(map[string]any{
		"npm:name":        pn.Original,
		"npm:version":     version,
		"npm:uploaded_at": uploadedAt,
	})); err != nil && !errors.Is(err, core.ErrAlreadyExists) {
		logging.FromContext(ctx).Warn("proxy cache fill add version failed",
			logging.KeyComponent, "npm", logging.KeyError, err)
		return
	}
	doc, ok := pkmt.Versions[version]
	if !ok {
		return
	}
	rewritten, err := json.Marshal(rewriteVersionTarball(doc, base, ns, pn))
	if err != nil {
		return
	}
	if _, err := store.Package(coreName).Version(version).AddFile(ctx, metadataFile, bytes.NewReader(rewritten), core.WithAllowOverwrite(true)); err != nil {
		logging.FromContext(ctx).Warn("proxy cache fill metadata write failed",
			logging.KeyComponent, "npm", logging.KeyError, err)
	}
}

// allow evaluates the namespace filter chain for a tarball download. npm
// publish times come from the packument's time map; when a delay filter needs a
// time the packument did not carry, the decision fails closed (denied).
func (e *proxyEngine) allow(r *http.Request, pn PackageName, version, filename string, spec namespace.Spec, pkmt *proxyPackument) bool {
	if len(spec.Proxy.Filters) == 0 {
		return true
	}
	chain, err := filter.Compile(spec.Proxy.Filters)
	if err != nil {
		logging.FromContext(r.Context()).Error("proxy filter compile failed",
			logging.KeyComponent, "npm", logging.KeyError, err)
		return false
	}
	ref := filter.Ref{Package: pn.Original, Version: version, PublishedAt: pkmt.publishTime(version)}
	dec := chain.DecideAt(ref, e.now())
	if dec.Allowed() {
		return true
	}
	logging.FromContext(r.Context()).Info("proxy tarball denied by filter",
		logging.KeyComponent, "npm", "package", pn.Original, "version", version,
		"filename", filename, "reason", dec.Reason)
	return false
}

// fetchPackument returns the upstream packument for pn through the two-level
// cache: the in-process memo first, then a singleflight-coalesced load.
func (e *proxyEngine) fetchPackument(ctx context.Context, ns string, store core.Store, spec namespace.Spec, pn PackageName) (*proxyPackument, error) {
	coreName := pn.Core()
	key := ns + "\x00" + coreName
	if p, ok := e.memo.get(key); ok {
		return p, nil
	}
	if e.neg.Has(ns, npmFormat, packumentKey(coreName)) {
		return nil, core.ErrNotFound
	}
	p, err, _ := e.sf.Do(key, func() (*proxyPackument, error) {
		if p, ok := e.memo.get(key); ok {
			return p, nil
		}
		return e.loadPackument(ctx, ns, store, spec, pn)
	})
	if err != nil {
		return nil, err
	}
	e.memo.put(key, p)
	return p, nil
}

// loadPackument serves a fresh durable snapshot when one exists, else fetches
// upstream and overwrites the snapshot. A clean upstream 404 is negative-cached;
// an unavailable or malformed upstream falls back to the stale snapshot or a
// synthesized packument.
func (e *proxyEngine) loadPackument(ctx context.Context, ns string, store core.Store, spec namespace.Spec, pn PackageName) (*proxyPackument, error) {
	coreName := pn.Core()
	key := packumentKey(coreName)
	cf := store.Cache(key)

	if e.durTTL > 0 {
		if exists, err := cf.Exists(ctx); err == nil && exists {
			if meta, err := cf.Meta(ctx); err == nil && e.now().Sub(meta.UpdatedAt) < e.durTTL {
				if p, err := readCachedPackument(ctx, cf); err == nil {
					return p, nil
				}
			}
		}
	}

	resp, err := e.httpc.Get(ctx, e.packumentURL(spec, pn))
	if err != nil {
		logging.FromContext(ctx).Warn("proxy packument fetch failed",
			logging.KeyComponent, "npm", logging.KeyError, err)
		return e.fallbackPackument(ctx, store, pn)
	}
	switch {
	case resp.IsNotFound():
		e.neg.Mark(ns, npmFormat, key)
		return nil, core.ErrNotFound
	case resp.IsOK():
		p, perr := parseUpstreamPackument(resp.Body, pn)
		if perr != nil {
			logging.FromContext(ctx).Warn("proxy packument parse failed",
				logging.KeyComponent, "npm", logging.KeyError, perr)
			return e.fallbackPackument(ctx, store, pn)
		}
		if b, err := json.Marshal(p); err == nil {
			if _, err := store.AddCache(ctx, key, bytes.NewReader(b)); err != nil {
				logging.FromContext(ctx).Warn("proxy packument cache write failed",
					logging.KeyComponent, "npm", logging.KeyError, err)
			}
		}
		e.neg.Delete(ns, npmFormat, key)
		return p, nil
	default:
		logging.FromContext(ctx).Warn("proxy packument upstream status",
			logging.KeyComponent, "npm", "status", resp.Status)
		return e.fallbackPackument(ctx, store, pn)
	}
}

// fallbackPackument is the upstream-unavailable path: serve the durable snapshot
// at any age, else a packument synthesized from locally cached tarballs, else
// report the upstream as unavailable (503).
func (e *proxyEngine) fallbackPackument(ctx context.Context, store core.Store, pn PackageName) (*proxyPackument, error) {
	cf := store.Cache(packumentKey(pn.Core()))
	if exists, err := cf.Exists(ctx); err == nil && exists {
		if p, err := readCachedPackument(ctx, cf); err == nil {
			return p, nil
		}
	}
	if syn, ok := e.synthesize(ctx, store, pn); ok {
		return syn, nil
	}
	return nil, errUpstreamUnavailable
}

// synthesize builds a minimal packument from the tarballs and package.json
// documents already cached in the Store, used when upstream is unavailable and
// no cached packument exists. Local dist-tags are used when present; otherwise
// dist-tags are omitted (proxy mode never sets them locally).
func (e *proxyEngine) synthesize(ctx context.Context, store core.Store, pn PackageName) (*proxyPackument, bool) {
	pkg := store.Package(pn.Core())
	if exists, err := pkg.Exists(ctx); err != nil || !exists {
		return nil, false
	}
	versions, err := pkg.Versions(ctx)
	if err != nil {
		return nil, false
	}
	p := &proxyPackument{Name: pn.Original, Versions: map[string]map[string]any{}, Time: map[string]string{}}
	for _, v := range versions {
		meta, uploadedAt, err := readVersionMetadata(ctx, v)
		if err != nil {
			continue
		}
		p.Versions[v.Name()] = meta
		if uploadedAt != "" {
			p.Time[v.Name()] = uploadedAt
		}
	}
	if len(p.Versions) == 0 {
		return nil, false
	}
	if len(p.Time) == 0 {
		p.Time = nil
	}
	if tags, err := pkg.TagTargets(ctx); err == nil && len(tags) > 0 {
		p.DistTags = tags
	}
	return p, true
}

func (e *proxyEngine) writeError(w http.ResponseWriter, r *http.Request, err error) {
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

// packumentURL builds the upstream packument URL. A scoped name uses the
// %2f-encoded single-segment form npm's registry expects
// (https://registry.npmjs.org/@scope%2fname); npm name rules already restrict
// the scope and name to URL-safe bytes, so no further escaping is needed.
func (e *proxyEngine) packumentURL(spec namespace.Spec, pn PackageName) string {
	base := upstreamBase(spec)
	if pn.Scoped() {
		return base + "/@" + pn.Scope + "%2f" + pn.Name
	}
	return base + "/" + pn.Name
}

func upstreamBase(spec namespace.Spec) string {
	return strings.TrimRight(spec.Proxy.Upstream, "/")
}

func packumentKey(coreName string) string { return "npm:packument:" + coreName }

func tarballKey(coreName, filename string) string {
	return "npm:tarball:" + coreName + ":" + filename
}

func readCachedPackument(ctx context.Context, cf core.CacheFile) (*proxyPackument, error) {
	rc, err := cf.Read(ctx)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	var p proxyPackument
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// parseUpstreamPackument decodes an upstream packument, keeping the full version
// documents (with upstream dist.tarball URLs intact) and the upstream dist-tags.
func parseUpstreamPackument(body []byte, pn PackageName) (*proxyPackument, error) {
	var raw struct {
		Name     string                    `json:"name"`
		DistTags map[string]string         `json:"dist-tags"`
		Versions map[string]map[string]any `json:"versions"`
		Time     map[string]string         `json:"time"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	if len(raw.Versions) == 0 {
		return nil, fmt.Errorf("npm: upstream packument has no versions")
	}
	return &proxyPackument{
		Name:     pn.Original,
		DistTags: raw.DistTags,
		Versions: raw.Versions,
		Time:     raw.Time,
	}, nil
}

// renderPackument assembles the served packument, rewriting every version's
// dist.tarball to this registry. It copies the maps it mutates so the cached
// proxyPackument (shared via the memo) is never modified.
func renderPackument(p *proxyPackument, base, ns string, pn PackageName) Packument {
	versions := make(map[string]any, len(p.Versions))
	for v, doc := range p.Versions {
		versions[v] = rewriteVersionTarball(doc, base, ns, pn)
	}
	tags := p.DistTags
	if tags == nil {
		tags = map[string]string{}
	}
	return Packument{
		ID:       pn.Original,
		Name:     pn.Original,
		DistTags: tags,
		Versions: versions,
		Time:     p.Time,
	}
}

// rewriteVersionTarball returns a shallow copy of a version document with
// dist.tarball pointed at this registry. The doc and its dist sub-map are copied
// rather than mutated so a shared (cached) document is left untouched.
func rewriteVersionTarball(doc map[string]any, base, ns string, pn PackageName) map[string]any {
	out := make(map[string]any, len(doc))
	for k, v := range doc {
		out[k] = v
	}
	dist, _ := doc["dist"].(map[string]any)
	if dist == nil {
		return out
	}
	nd := make(map[string]any, len(dist))
	for k, v := range dist {
		nd[k] = v
	}
	if t, _ := nd["tarball"].(string); t != "" {
		if filename := path.Base(t); filename != "" && filename != "." && filename != "/" {
			nd["tarball"] = tarballURL(base, ns, pn, filename)
		}
	}
	out["dist"] = nd
	return out
}
