package debian

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yolocs/open-artifact/pkg/core"
	"github.com/yolocs/open-artifact/pkg/logging"
	"github.com/yolocs/open-artifact/pkg/namespace"
	"github.com/yolocs/open-artifact/pkg/observability"
	"github.com/yolocs/open-artifact/pkg/proxy/filter"
	"github.com/yolocs/open-artifact/pkg/proxy/httpclient"
	"github.com/yolocs/open-artifact/pkg/proxy/negcache"
	"github.com/yolocs/open-artifact/pkg/surface"
)

// proxyEngine holds the pull-through machinery for proxy-mode Debian namespaces.
//
// Index files (dists/...) are served verbatim: fetched from upstream on each
// request, streamed straight through to the client while teed into the durable
// .cache/ entry, and served from that cached copy only as a stale fallback when
// upstream is unavailable (the PyPI proxy shape). They are never parsed,
// rewritten, or filtered — APT verifies them end-to-end against the client's
// GPG key. Pool artifacts (.deb/.dsc/.tar.*) are cached as real Files: streamed
// through with a tee into the Store on a cold miss and served locally
// thereafter, subject to the namespace filter chain.
type proxyEngine struct {
	now   func() time.Time
	httpc *httpclient.Client
	neg   *negcache.Cache
}

func newProxyEngine(cfg Config, now func() time.Time) *proxyEngine {
	return &proxyEngine{
		now:   now,
		httpc: httpclient.New(),
		neg:   negcache.New(cfg.ProxyNegativeCacheTTL),
	}
}

// serve dispatches a proxy-mode download by parsed path kind.
func (e *proxyEngine) serve(w http.ResponseWriter, r *http.Request, p requestPath, ns string, spec namespace.Spec, store core.Store) {
	switch p.Kind {
	case pathPool:
		e.serveArtifact(w, r, ns, spec, store, p)
	default:
		e.serveIndex(w, r, ns, spec, store, p)
	}
}

// serveIndex pulls an index file from upstream and caches it, falling back to
// the durable cached copy when upstream is unavailable.
func (e *proxyEngine) serveIndex(w http.ResponseWriter, r *http.Request, ns string, spec namespace.Spec, store core.Store, p requestPath) {
	if e.neg.Has(ns, debianFormat, indexNegKey(p)) {
		surface.WriteStoreError(w, r, core.ErrNotFound)
		return
	}
	url := upstreamURL(spec, p)

	if r.Method == http.MethodHead {
		e.headIndex(w, r, ns, spec, store, p, url)
		return
	}

	sr, err := e.httpc.Stream(r.Context(), url)
	if err != nil {
		if serveCachedIndex(w, r, store, p) {
			return
		}
		e.writeUpstreamUnavailable(w, r, err)
		return
	}
	defer sr.Body.Close()

	switch {
	case sr.IsNotFound():
		e.neg.Mark(ns, debianFormat, indexNegKey(p))
		surface.WriteStoreError(w, r, core.ErrNotFound)
		return
	case !sr.IsOK():
		// 5xx or any other non-2xx: prefer a stale cached copy over an error.
		if serveCachedIndex(w, r, store, p) {
			return
		}
		if sr.IsServerError() {
			e.writeUpstreamUnavailable(w, r, nil)
			return
		}
		surface.WriteError(w, http.StatusBadGateway, "upstream index error")
		return
	}

	ct := indexContentType(p.RestRaw)
	copyErr, fillErr := surface.TeeStreamToStore(w, sr.Body, ct, sr.ContentLength, func(src io.Reader) error {
		_, err := store.AddCache(r.Context(), indexCacheKey(p), src)
		return err
	})
	switch {
	case copyErr != nil:
		logging.FromContext(r.Context()).Warn("proxy index stream interrupted",
			logging.KeyComponent, "debian", "path", p.RestRaw, logging.KeyError, copyErr)
	case fillErr != nil:
		logging.FromContext(r.Context()).Warn("proxy index cache fill failed",
			logging.KeyComponent, "debian", "path", p.RestRaw, logging.KeyError, fillErr)
	}
}

// headIndex answers a HEAD with an upstream HEAD, falling back to the cached
// copy's existence when upstream is unavailable.
func (e *proxyEngine) headIndex(w http.ResponseWriter, r *http.Request, ns string, spec namespace.Spec, store core.Store, p requestPath, url string) {
	resp, err := e.httpc.Head(r.Context(), url)
	if err != nil {
		if serveCachedIndex(w, r, store, p) {
			return
		}
		e.writeUpstreamUnavailable(w, r, err)
		return
	}
	switch {
	case resp.IsNotFound():
		e.neg.Mark(ns, debianFormat, indexNegKey(p))
		surface.WriteStoreError(w, r, core.ErrNotFound)
	case resp.IsOK():
		w.Header().Set("Content-Type", indexContentType(p.RestRaw))
		w.WriteHeader(http.StatusOK)
	default:
		if serveCachedIndex(w, r, store, p) {
			return
		}
		e.writeUpstreamUnavailable(w, r, nil)
	}
}

// serveArtifact serves a pool artifact: local cache hit, negative-cache check,
// filter evaluation, then an upstream stream-with-tee into the Store.
func (e *proxyEngine) serveArtifact(w http.ResponseWriter, r *http.Request, ns string, spec namespace.Spec, store core.Store, p requestPath) {
	local := store.Package(p.PoolDir).File(p.File)
	if exists, err := local.Exists(r.Context()); err != nil {
		surface.WriteStoreError(w, r, err)
		return
	} else if exists {
		surface.RedirectOrStreamFile(w, r, local, poolContentType(p.File))
		return
	}

	if e.neg.Has(ns, debianFormat, poolNegKey(p)) {
		surface.WriteStoreError(w, r, core.ErrNotFound)
		return
	}

	url := upstreamURL(spec, p)

	if r.Method == http.MethodHead {
		e.headArtifact(w, r, ns, url, p)
		return
	}

	if !e.allow(r, spec, p) {
		// A denied artifact looks like 404 to the client; the reason is logged.
		surface.WriteStoreError(w, r, core.ErrNotFound)
		return
	}

	sr, err := e.httpc.Stream(r.Context(), url)
	if err != nil {
		logging.FromContext(r.Context()).Error("proxy artifact fetch failed",
			logging.KeyComponent, "debian", logging.KeyError, err)
		surface.WriteError(w, http.StatusBadGateway, "upstream fetch failed")
		return
	}
	defer sr.Body.Close()
	switch {
	case sr.IsNotFound():
		e.neg.Mark(ns, debianFormat, poolNegKey(p))
		surface.WriteStoreError(w, r, core.ErrNotFound)
		return
	case !sr.IsOK():
		logging.FromContext(r.Context()).Error("proxy artifact upstream status",
			logging.KeyComponent, "debian", "status", sr.Status)
		surface.WriteError(w, http.StatusBadGateway, "upstream unavailable")
		return
	}

	e.streamAndCache(w, r, store, p, url, sr)
}

// headArtifact answers a cold HEAD with an upstream HEAD without caching bytes.
func (e *proxyEngine) headArtifact(w http.ResponseWriter, r *http.Request, ns, url string, p requestPath) {
	resp, err := e.httpc.Head(r.Context(), url)
	if err != nil {
		surface.WriteError(w, http.StatusBadGateway, "upstream fetch failed")
		return
	}
	switch {
	case resp.IsNotFound():
		e.neg.Mark(ns, debianFormat, poolNegKey(p))
		surface.WriteStoreError(w, r, core.ErrNotFound)
	case resp.IsOK():
		w.Header().Set("Content-Type", poolContentType(p.File))
		w.WriteHeader(http.StatusOK)
	default:
		surface.WriteError(w, http.StatusBadGateway, "upstream unavailable")
	}
}

// streamAndCache streams the upstream artifact to the client while teeing the
// same bytes into the Store as a real File. On a clean fill it records the
// package envelope so the cache survives a process restart.
func (e *proxyEngine) streamAndCache(w http.ResponseWriter, r *http.Request, store core.Store, p requestPath, upstream string, sr *httpclient.StreamResponse) {
	ctx := r.Context()
	ann := artifactAnnotations(p, e.now().UTC(), upstream)

	copyErr, fillErr := surface.TeeStreamToStore(w, sr.Body, poolContentType(p.File), sr.ContentLength, func(src io.Reader) error {
		_, err := store.Package(p.PoolDir).AddFile(ctx, p.File, src, core.WithAnnotations(ann))
		return err
	})
	switch {
	case copyErr != nil:
		logging.FromContext(ctx).Warn("proxy artifact stream interrupted",
			logging.KeyComponent, "debian", "pool_dir", p.PoolDir, "file", p.File, logging.KeyError, copyErr)
	case fillErr != nil:
		logging.FromContext(ctx).Warn("proxy cache fill failed",
			logging.KeyComponent, "debian", "pool_dir", p.PoolDir, "file", p.File, logging.KeyError, fillErr)
	default:
		if _, err := store.AddPackage(ctx, p.PoolDir, core.WithAnnotations(map[string]any{
			"debian:pool_dir": p.PoolDir,
			"debian:package":  p.PkgName,
		})); err != nil && !errors.Is(err, core.ErrAlreadyExists) {
			logging.FromContext(ctx).Warn("proxy cache fill add package failed",
				logging.KeyComponent, "debian", logging.KeyError, err)
		}
	}
}

// allow evaluates the namespace filter chain for an artifact download. The Ref
// uses the parsed package name and version. Debian exposes no reliable per-file
// publish time, so a delay filter that needs one fails closed (denied).
func (e *proxyEngine) allow(r *http.Request, spec namespace.Spec, p requestPath) bool {
	if len(spec.Proxy.Filters) == 0 {
		return true
	}
	chain, err := filter.Compile(spec.Proxy.Filters)
	if err != nil {
		logging.FromContext(r.Context()).Error("proxy filter compile failed",
			logging.KeyComponent, "debian", logging.KeyError, err)
		return false
	}
	ref := filter.Ref{Package: p.PkgName, Version: p.Version}
	dec := chain.DecideAt(ref, e.now())
	if dec.NeedsMetadata() {
		logging.FromContext(r.Context()).Info("proxy artifact denied: publish time unknown for delay filter",
			logging.KeyComponent, "debian", "package", p.PkgName, "version", p.Version)
		return false
	}
	if dec.Allowed() {
		return true
	}
	logging.FromContext(r.Context()).Info("proxy artifact denied by filter",
		logging.KeyComponent, "debian", "package", p.PkgName, "version", p.Version,
		"file", p.File, "reason", dec.Reason)
	return false
}

func (e *proxyEngine) writeUpstreamUnavailable(w http.ResponseWriter, r *http.Request, err error) {
	if err != nil {
		logging.FromContext(r.Context()).Warn("proxy upstream unavailable",
			logging.KeyComponent, "debian", logging.KeyError, err)
	}
	observability.RecordError(r, errUpstreamUnavailable)
	surface.WriteError(w, http.StatusServiceUnavailable, "upstream unavailable")
}

// serveCachedIndex serves the durable cached copy of an index file when
// present, returning true if it handled the response. It is the stale fallback
// used when upstream is unavailable and the local read path for hosted
// namespaces. Large indexes are streamed, never buffered.
func serveCachedIndex(w http.ResponseWriter, r *http.Request, store core.Store, p requestPath) bool {
	ctx := r.Context()
	cf := store.Cache(indexCacheKey(p))
	exists, err := cf.Exists(ctx)
	if err != nil || !exists {
		return false
	}
	w.Header().Set("Content-Type", indexContentType(p.RestRaw))
	if meta, err := cf.Meta(ctx); err == nil && meta.Size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return true
	}
	rc, err := cf.Read(ctx)
	if err != nil {
		return false
	}
	defer rc.Close()
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
	return true
}

func artifactAnnotations(p requestPath, fetchedAt time.Time, upstream string) map[string]any {
	out := map[string]any{
		"debian:file":        p.File,
		"debian:pool_dir":    p.PoolDir,
		"debian:package":     p.PkgName,
		"debian:version":     p.Version,
		"debian:uploaded_at": fetchedAt.Format(time.RFC3339Nano),
	}
	if upstream != "" {
		out["debian:upstream_url"] = upstream
	}
	return out
}

// upstreamURL builds the upstream URL for a request: the namespace's upstream
// base (trailing slash trimmed) joined with the still-escaped repo-relative
// path, so the bytes APT verifies are forwarded unchanged.
func upstreamURL(spec namespace.Spec, p requestPath) string {
	return strings.TrimRight(spec.Proxy.Upstream, "/") + "/" + p.RestRaw
}

// errUpstreamUnavailable is recorded for observability when an upstream fetch
// cannot be served (transport failure or 5xx) and no cached copy exists. It
// maps to 503.
var errUpstreamUnavailable = errors.New("debian: upstream unavailable")
