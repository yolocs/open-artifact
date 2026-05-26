package maven

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
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

// mavenLastUpdatedLayout is the format of the <lastUpdated> field inside Maven
// metadata (UTC, no separators), used as a coarse upload time for delay
// filters.
const mavenLastUpdatedLayout = "20060102150405"

// proxyEngine holds the pull-through machinery for proxy-mode Maven namespaces.
//
// Unlike the npm/PyPI proxies, Maven metadata is never cached or synthesized in
// v1: maven-metadata.xml (artifact- and snapshot-level) and its checksum
// companions are fetched live from upstream on every request and streamed
// through unchanged, so a client always sees authoritative versioning. Only
// immutable artifact bytes (.jar/.pom/sources/javadoc and their checksums) are
// cached: pulled on first request as real Files and served from our Store
// thereafter. The negative cache bounds repeated upstream 404s.
type proxyEngine struct {
	now   func() time.Time
	httpc *httpclient.Client
	neg   *negcache.Cache
}

func newProxyEngine(cfg Config, now func() time.Time) *proxyEngine {
	return &proxyEngine{
		now:   now,
		httpc: httpclient.New(httpclient.WithMaxBodyBytes(cfg.proxyMetadataMaxBytes())),
		neg:   negcache.New(cfg.ProxyNegativeCacheTTL),
	}
}

// serve dispatches a proxy-mode download by parsed path kind.
func (e *proxyEngine) serve(w http.ResponseWriter, r *http.Request, p requestPath, ns string, spec namespace.Spec, store core.Store) {
	switch p.Kind {
	case pathArtifactMetadata, pathVersionMetadata:
		e.serveMetadata(w, r, ns, spec, p)
	case pathArchetypeCatalog:
		e.serveArchetypeCatalog(w, r, store, p)
	default:
		e.serveArtifact(w, r, ns, spec, store, p)
	}
}

// serveMetadata is the live metadata passthrough: it never reads or writes the
// Store. A negatively cached coordinate short-circuits to 404; otherwise the
// upstream document (or its checksum companion) is fetched and streamed through
// with the original status. Oversized or otherwise malformed responses map to
// 502; an unreachable or 5xx upstream maps to 503.
func (e *proxyEngine) serveMetadata(w http.ResponseWriter, r *http.Request, ns string, spec namespace.Spec, p requestPath) {
	if e.neg.Has(ns, mavenFormat, metadataNegKey(p)) {
		surface.WriteStoreError(w, r, core.ErrNotFound)
		return
	}
	url := upstreamURL(spec, p)

	if r.Method == http.MethodHead {
		resp, err := e.httpc.Head(r.Context(), url)
		if err != nil {
			e.writeUpstreamUnavailable(w, r, "maven", err)
			return
		}
		switch {
		case resp.IsNotFound():
			e.neg.Mark(ns, mavenFormat, metadataNegKey(p))
			surface.WriteStoreError(w, r, core.ErrNotFound)
		case resp.IsOK():
			w.Header().Set("Content-Type", proxyContentType(p))
			w.WriteHeader(http.StatusOK)
		case resp.IsServerError():
			e.writeUpstreamUnavailable(w, r, "maven", nil)
		default:
			surface.WriteError(w, http.StatusBadGateway, "upstream metadata error")
		}
		return
	}

	resp, err := e.httpc.Get(r.Context(), url)
	if err != nil {
		if errors.Is(err, httpclient.ErrOversized) {
			logging.FromContext(r.Context()).Warn("proxy metadata oversized",
				logging.KeyComponent, "maven", logging.KeyError, err)
			surface.WriteError(w, http.StatusBadGateway, "upstream metadata too large")
			return
		}
		e.writeUpstreamUnavailable(w, r, "maven", err)
		return
	}
	switch {
	case resp.IsNotFound():
		e.neg.Mark(ns, mavenFormat, metadataNegKey(p))
		surface.WriteStoreError(w, r, core.ErrNotFound)
	case resp.IsOK():
		writeBytes(w, proxyContentType(p), resp.Body)
	case resp.IsServerError():
		e.writeUpstreamUnavailable(w, r, "maven", nil)
	default:
		surface.WriteError(w, http.StatusBadGateway, "upstream metadata error")
	}
}

// serveArchetypeCatalog serves /archetype-catalog.xml in proxy mode. V1 is
// local-cache-only: a missing catalog is a 404, never an upstream fetch.
func (e *proxyEngine) serveArchetypeCatalog(w http.ResponseWriter, r *http.Request, store core.Store, p requestPath) {
	f := store.File(p.File)
	exists, err := f.Exists(r.Context())
	if err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	if !exists {
		surface.WriteStoreError(w, r, core.ErrNotFound)
		return
	}
	surface.RedirectOrStreamFile(w, r, f, "application/xml")
}

// serveArtifact serves an artifact file (or its checksum companion): local
// cache hit, else negative-cache check, filter evaluation, and an upstream
// stream-with-tee into the Store.
func (e *proxyEngine) serveArtifact(w http.ResponseWriter, r *http.Request, ns string, spec namespace.Spec, store core.Store, p requestPath) {
	local := store.Package(p.Package).Version(p.Version).File(p.File)
	if exists, err := local.Exists(r.Context()); err != nil {
		surface.WriteStoreError(w, r, err)
		return
	} else if exists {
		surface.RedirectOrStreamFile(w, r, local, proxyContentType(p))
		return
	}

	if e.neg.Has(ns, mavenFormat, fileNegKey(p)) {
		surface.WriteStoreError(w, r, core.ErrNotFound)
		return
	}

	if p.Checksum != checksumNone {
		e.serveChecksum(w, r, ns, spec, store, p)
		return
	}

	if r.Method == http.MethodHead {
		e.headArtifact(w, r, ns, spec, p)
		return
	}

	if !e.allow(r, spec, p) {
		// A denied artifact looks like 404 to the client; the reason is logged.
		surface.WriteStoreError(w, r, core.ErrNotFound)
		return
	}

	url := upstreamURL(spec, p)
	sr, err := e.httpc.Stream(r.Context(), url)
	if err != nil {
		logging.FromContext(r.Context()).Error("proxy artifact fetch failed",
			logging.KeyComponent, "maven", logging.KeyError, err)
		surface.WriteError(w, http.StatusBadGateway, "upstream fetch failed")
		return
	}
	defer sr.Body.Close()
	switch {
	case sr.IsNotFound():
		e.neg.Mark(ns, mavenFormat, fileNegKey(p))
		surface.WriteStoreError(w, r, core.ErrNotFound)
		return
	case !sr.IsOK():
		logging.FromContext(r.Context()).Error("proxy artifact upstream status",
			logging.KeyComponent, "maven", "status", sr.Status)
		surface.WriteError(w, http.StatusBadGateway, "upstream unavailable")
		return
	}

	e.streamAndCache(w, r, store, p, url, sr)
}

// headArtifact answers a cold HEAD with an upstream HEAD and does not cache
// bytes, matching the spec's allowance for unconditional HEAD passthrough.
func (e *proxyEngine) headArtifact(w http.ResponseWriter, r *http.Request, ns string, spec namespace.Spec, p requestPath) {
	resp, err := e.httpc.Head(r.Context(), upstreamURL(spec, p))
	if err != nil {
		surface.WriteError(w, http.StatusBadGateway, "upstream fetch failed")
		return
	}
	switch {
	case resp.IsNotFound():
		e.neg.Mark(ns, mavenFormat, fileNegKey(p))
		surface.WriteStoreError(w, r, core.ErrNotFound)
	case resp.IsOK():
		w.Header().Set("Content-Type", proxyContentType(p))
		w.WriteHeader(http.StatusOK)
	default:
		surface.WriteError(w, http.StatusBadGateway, "upstream unavailable")
	}
}

// serveChecksum fills a checksum companion. When the target artifact is already
// cached locally, the checksum is synthesized from the cached bytes and cached
// itself; otherwise it is fetched from upstream and cached as a normal file.
func (e *proxyEngine) serveChecksum(w http.ResponseWriter, r *http.Request, ns string, spec namespace.Spec, store core.Store, p requestPath) {
	ctx := r.Context()
	target := store.Package(p.Package).Version(p.Version).File(p.TargetFile)
	cached, err := target.Exists(ctx)
	if err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}

	if cached {
		sum, err := computeChecksum(ctx, p.Checksum, target)
		if err != nil {
			surface.WriteStoreError(w, r, err)
			return
		}
		if _, err := store.Package(p.Package).Version(p.Version).AddFile(ctx, p.File, strings.NewReader(sum), core.WithAnnotations(checksumAnnotations(p, e.now().UTC()))); err != nil && !errors.Is(err, core.ErrAlreadyExists) {
			logging.FromContext(ctx).Warn("proxy checksum cache write failed",
				logging.KeyComponent, "maven", logging.KeyError, err)
		}
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Type", proxyContentType(p))
			w.WriteHeader(http.StatusOK)
			return
		}
		writeBytes(w, proxyContentType(p), []byte(sum))
		return
	}

	if r.Method == http.MethodHead {
		e.headArtifact(w, r, ns, spec, p)
		return
	}

	resp, err := e.httpc.Get(ctx, upstreamURL(spec, p))
	if err != nil {
		logging.FromContext(ctx).Error("proxy checksum fetch failed",
			logging.KeyComponent, "maven", logging.KeyError, err)
		surface.WriteError(w, http.StatusBadGateway, "upstream fetch failed")
		return
	}
	switch {
	case resp.IsNotFound():
		e.neg.Mark(ns, mavenFormat, fileNegKey(p))
		surface.WriteStoreError(w, r, core.ErrNotFound)
	case resp.IsOK():
		e.recordParents(ctx, store, p)
		if _, err := store.Package(p.Package).Version(p.Version).AddFile(ctx, p.File, bytes.NewReader(resp.Body), core.WithAnnotations(checksumAnnotations(p, e.now().UTC()))); err != nil && !errors.Is(err, core.ErrAlreadyExists) {
			logging.FromContext(ctx).Warn("proxy checksum cache write failed",
				logging.KeyComponent, "maven", logging.KeyError, err)
		}
		writeBytes(w, proxyContentType(p), resp.Body)
	default:
		surface.WriteError(w, http.StatusBadGateway, "upstream unavailable")
	}
}

// streamAndCache streams the upstream artifact to the client while teeing the
// same bytes into the Store as a real File. On a clean fill it records the
// package/version envelopes so the cache survives a process restart.
func (e *proxyEngine) streamAndCache(w http.ResponseWriter, r *http.Request, store core.Store, p requestPath, upstream string, sr *httpclient.StreamResponse) {
	ctx := r.Context()
	ann := artifactAnnotations(p, e.now().UTC(), upstream)

	copyErr, fillErr := surface.TeeStreamToStore(w, sr.Body, proxyContentType(p), sr.ContentLength, func(src io.Reader) error {
		_, err := store.Package(p.Package).Version(p.Version).AddFile(ctx, p.File, src, core.WithAnnotations(ann))
		return err
	})
	switch {
	case copyErr != nil:
		logging.FromContext(ctx).Warn("proxy artifact stream interrupted",
			logging.KeyComponent, "maven", "package", p.Package, "version", p.Version,
			"file", p.File, logging.KeyError, copyErr)
	case fillErr != nil:
		logging.FromContext(ctx).Warn("proxy cache fill failed",
			logging.KeyComponent, "maven", "package", p.Package, "version", p.Version,
			"file", p.File, logging.KeyError, fillErr)
	default:
		e.recordParents(ctx, store, p)
	}
}

// recordParents writes the package and version envelopes after a successful
// fill, marking the package index for groupId:artifactId. Best-effort: a
// failure is logged and the next request refills.
func (e *proxyEngine) recordParents(ctx context.Context, store core.Store, p requestPath) {
	if _, err := store.AddPackage(ctx, p.Package, core.WithAnnotations(map[string]any{
		"maven:group_id":    p.GroupID,
		"maven:artifact_id": p.ArtifactID,
	})); err != nil && !errors.Is(err, core.ErrAlreadyExists) {
		logging.FromContext(ctx).Warn("proxy cache fill add package failed",
			logging.KeyComponent, "maven", logging.KeyError, err)
		return
	}
	if _, err := store.Package(p.Package).AddVersion(ctx, p.Version, core.WithAnnotations(map[string]any{
		"maven:version": p.Version,
	})); err != nil && !errors.Is(err, core.ErrAlreadyExists) {
		logging.FromContext(ctx).Warn("proxy cache fill add version failed",
			logging.KeyComponent, "maven", logging.KeyError, err)
	}
}

// allow evaluates the namespace filter chain for an artifact download. The
// package ref is groupId:artifactId with the route version. A delay filter that
// needs a publish time triggers a live artifact-metadata fetch to read
// <versioning><lastUpdated>; if that is still unavailable the decision fails
// closed (denied).
func (e *proxyEngine) allow(r *http.Request, spec namespace.Spec, p requestPath) bool {
	if len(spec.Proxy.Filters) == 0 {
		return true
	}
	chain, err := filter.Compile(spec.Proxy.Filters)
	if err != nil {
		logging.FromContext(r.Context()).Error("proxy filter compile failed",
			logging.KeyComponent, "maven", logging.KeyError, err)
		return false
	}
	ref := filter.Ref{Package: p.GroupID + ":" + p.ArtifactID, Version: p.Version}
	dec := chain.DecideAt(ref, e.now())
	if dec.NeedsMetadata() {
		if t := e.lastUpdated(r.Context(), spec, p); t != nil {
			ref.PublishedAt = t
			dec = chain.DecideAt(ref, e.now())
		} else {
			logging.FromContext(r.Context()).Info("proxy artifact denied: publish time unknown for delay filter",
				logging.KeyComponent, "maven", "package", ref.Package, "version", p.Version)
			return false
		}
	}
	if dec.Allowed() {
		return true
	}
	logging.FromContext(r.Context()).Info("proxy artifact denied by filter",
		logging.KeyComponent, "maven", "package", ref.Package, "version", p.Version,
		"file", p.File, "reason", dec.Reason)
	return false
}

// lastUpdated fetches the artifact-level metadata and parses its
// <versioning><lastUpdated> as a coarse upload time. It returns nil when the
// metadata is unavailable, malformed, or carries no parsable timestamp.
func (e *proxyEngine) lastUpdated(ctx context.Context, spec namespace.Spec, p requestPath) *time.Time {
	resp, err := e.httpc.Get(ctx, upstreamMetadataURL(spec, p))
	if err != nil || !resp.IsOK() {
		return nil
	}
	var md struct {
		Versioning struct {
			LastUpdated string `xml:"lastUpdated"`
		} `xml:"versioning"`
	}
	if err := xml.Unmarshal(resp.Body, &md); err != nil {
		return nil
	}
	s := strings.TrimSpace(md.Versioning.LastUpdated)
	if s == "" {
		return nil
	}
	t, err := time.Parse(mavenLastUpdatedLayout, s)
	if err != nil {
		return nil
	}
	t = t.UTC()
	return &t
}

func (e *proxyEngine) writeUpstreamUnavailable(w http.ResponseWriter, r *http.Request, component string, err error) {
	if err != nil {
		logging.FromContext(r.Context()).Warn("proxy upstream unavailable",
			logging.KeyComponent, component, logging.KeyError, err)
	}
	observability.RecordError(r, errUpstreamUnavailable)
	surface.WriteError(w, http.StatusServiceUnavailable, "upstream unavailable")
}

// errUpstreamUnavailable is recorded for observability when an upstream
// metadata fetch cannot be served (transport failure or 5xx). It maps to 503.
var errUpstreamUnavailable = errors.New("maven: upstream unavailable")

// computeChecksum reads the cached target file and returns its bare lowercase
// hex digest in the requested algorithm — the exact body a Maven client expects
// in a .md5/.sha1/.sha256/.sha512 companion.
func computeChecksum(ctx context.Context, alg checksumAlgorithm, f core.File) (string, error) {
	h, err := hashForChecksum(alg)
	if err != nil {
		return "", err
	}
	rc, err := f.Read(ctx)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	if _, err := io.Copy(h, rc); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeBytes(w http.ResponseWriter, contentType string, body []byte) {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// proxyContentType picks the served Content-Type for a proxy artifact/metadata
// response: checksum companions are text, metadata XML is application/xml, and
// everything else (jars, poms, sources/javadoc) is opaque bytes.
func proxyContentType(p requestPath) string {
	switch {
	case p.Checksum != checksumNone:
		return "text/plain; charset=utf-8"
	case p.File == metadataFile:
		return "application/xml"
	default:
		return "application/octet-stream"
	}
}

func artifactAnnotations(p requestPath, fetchedAt time.Time, upstream string) map[string]any {
	out := map[string]any{
		"maven:file":        p.File,
		"maven:group_id":    p.GroupID,
		"maven:artifact_id": p.ArtifactID,
		"maven:version":     p.Version,
		"maven:uploaded_at": fetchedAt.Format(time.RFC3339Nano),
	}
	if upstream != "" {
		out["maven:upstream_url"] = upstream
	}
	return out
}

func checksumAnnotations(p requestPath, fetchedAt time.Time) map[string]any {
	out := artifactAnnotations(p, fetchedAt, "")
	out["maven:checksum"] = string(p.Checksum)
	out["maven:checksum_target"] = p.TargetFile
	return out
}

// upstreamURL builds the upstream URL for a coordinate request: the namespace's
// upstream base (trailing slash trimmed) joined with the request path after
// /{namespace}/maven2/.
func upstreamURL(spec namespace.Spec, p requestPath) string {
	return upstreamBase(spec) + "/" + coordinatePath(p)
}

// upstreamMetadataURL builds the artifact-level maven-metadata.xml URL for a
// coordinate, used to read a coarse publish time for delay filters.
func upstreamMetadataURL(spec namespace.Spec, p requestPath) string {
	segs := append([]string{}, p.GroupPath...)
	segs = append(segs, p.ArtifactID, metadataFile)
	return upstreamBase(spec) + "/" + strings.Join(segs, "/")
}

// coordinatePath reconstructs the upstream-relative path from the parsed
// coordinate: group/.../artifactId[/version]/file. Version is empty for
// artifact-level metadata.
func coordinatePath(p requestPath) string {
	segs := append([]string{}, p.GroupPath...)
	segs = append(segs, p.ArtifactID)
	if p.Version != "" {
		segs = append(segs, p.Version)
	}
	segs = append(segs, p.File)
	return strings.Join(segs, "/")
}

func upstreamBase(spec namespace.Spec) string {
	return strings.TrimRight(spec.Proxy.Upstream, "/")
}

func fileNegKey(p requestPath) string {
	return "maven:file:" + p.GroupID + ":" + p.ArtifactID + ":" + p.Version + ":" + p.File
}

func metadataNegKey(p requestPath) string {
	return "maven:metadata:" + p.GroupID + ":" + p.ArtifactID
}
