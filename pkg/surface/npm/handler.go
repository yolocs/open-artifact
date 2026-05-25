package npm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"

	"github.com/yolocs/open-artifact/pkg/auth"
	"github.com/yolocs/open-artifact/pkg/core"
	"github.com/yolocs/open-artifact/pkg/namespace"
	"github.com/yolocs/open-artifact/pkg/observability"
	"github.com/yolocs/open-artifact/pkg/surface"
)

const npmFormat = string(core.FormatNPM)

// metadataFile is the per-version file that stores the publisher's version
// metadata document (the value of versions[<version>] in the publish payload),
// used to reconstruct packuments.
const metadataFile = "package.json"

// DefaultMaxUploadBytes caps the publish request body. npm wraps the tarball as
// base64 inside a JSON document, so the body is ~1.37x the tarball size.
const DefaultMaxUploadBytes int64 = 100 << 20

// Proxy-mode defaults. The packument memo is the in-process burst absorber in
// front of the durable upstream-packument snapshot; the durable snapshot is
// served while younger than the cache TTL and as a stale fallback when upstream
// is unavailable; the negative TTL bounds how long an upstream 404 is
// remembered. Tarball bytes are streamed straight through (tee-to-store), so
// there is no artifact buffer to cap.
const (
	DefaultProxyPackumentMemoTTL  = 10 * time.Second
	DefaultProxyPackumentCacheTTL = 10 * time.Minute
	DefaultProxyNegativeCacheTTL  = 30 * time.Second
)

// versionReadConcurrency bounds the per-version metadata fan-out when assembling
// a packument so a package with many versions does not open an unbounded number
// of simultaneous backend reads.
const versionReadConcurrency = 16

type Config struct {
	MaxUploadBytes int64

	// Proxy-mode knobs. A zero value falls back to the matching Default above.
	ProxyPackumentMemoTTL  time.Duration
	ProxyPackumentCacheTTL time.Duration
	ProxyNegativeCacheTTL  time.Duration
}

func (c Config) uploadLimit() int64 {
	if c.MaxUploadBytes <= 0 {
		return DefaultMaxUploadBytes
	}
	return c.MaxUploadBytes
}

// proxyPackumentMemoTTL resolves the in-process packument memo TTL: zero means
// the default, a negative value disables the memo (so every request re-resolves
// through the durable cache, which lets tests force re-resolution).
func (c Config) proxyPackumentMemoTTL() time.Duration {
	if c.ProxyPackumentMemoTTL == 0 {
		return DefaultProxyPackumentMemoTTL
	}
	return c.ProxyPackumentMemoTTL
}

// proxyPackumentCacheTTL resolves the durable packument freshness window: zero
// means the default, a negative value means the durable snapshot is never
// served as fresh (every miss past the memo refetches upstream, the snapshot
// serving only as a stale fallback — matching the PyPI proxy and letting tests
// force upstream refetch).
func (c Config) proxyPackumentCacheTTL() time.Duration {
	if c.ProxyPackumentCacheTTL == 0 {
		return DefaultProxyPackumentCacheTTL
	}
	return c.ProxyPackumentCacheTTL
}

// Handler builds the npm registry surface. It composes namespace lookup and
// authorization, hosted/proxy dispatch, and the shared error/redirect helpers
// exactly like the PyPI surface; only the wire protocol and codec are
// npm-specific.
func Handler(reg *namespace.Registry, authn auth.Authenticator, cfg Config) http.Handler {
	now := time.Now
	h := &handler{reg: reg, opts: cfg, now: now, proxy: newProxyEngine(cfg, now)}
	return auth.Middleware(authn)(h.router())
}

type handler struct {
	reg   *namespace.Registry
	opts  Config
	now   func() time.Time
	proxy *proxyEngine
}

func (h *handler) router() http.Handler {
	r := mux.NewRouter()

	// Probe routes.
	r.HandleFunc("/{namespace}/-/ping", h.pingRoute).Methods(http.MethodGet, http.MethodHead)
	r.HandleFunc("/{namespace}", h.rootRoute).Methods(http.MethodGet, http.MethodHead)
	r.HandleFunc("/{namespace}/", h.rootRoute).Methods(http.MethodGet, http.MethodHead)

	// Dist-tag routes (more specific "-/package" prefix registered before the
	// generic packument routes).
	r.HandleFunc("/{namespace}/-/package/@{scope}/{name}/dist-tags", h.distTagsListRoute).Methods(http.MethodGet, http.MethodHead)
	r.HandleFunc("/{namespace}/-/package/{name}/dist-tags", h.distTagsListRoute).Methods(http.MethodGet, http.MethodHead)
	r.HandleFunc("/{namespace}/-/package/@{scope}/{name}/dist-tags/{tag}", h.distTagsRoute).Methods(http.MethodPut, http.MethodPost, http.MethodDelete)
	r.HandleFunc("/{namespace}/-/package/{name}/dist-tags/{tag}", h.distTagsRoute).Methods(http.MethodPut, http.MethodPost, http.MethodDelete)

	// Tarball downloads.
	r.HandleFunc("/{namespace}/@{scope}/{name}/-/{filename}", h.tarballRoute).Methods(http.MethodGet, http.MethodHead)
	r.HandleFunc("/{namespace}/{name}/-/{filename}", h.tarballRoute).Methods(http.MethodGet, http.MethodHead)

	// Packument read + publish.
	r.HandleFunc("/{namespace}/@{scope}/{name}", h.packageRoute).Methods(http.MethodGet, http.MethodHead, http.MethodPut)
	r.HandleFunc("/{namespace}/{name}", h.packageRoute).Methods(http.MethodGet, http.MethodHead, http.MethodPut)

	r.MethodNotAllowedHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		surface.WriteMethodNotAllowed(w, []string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPost})
	})
	return r
}

// namespace validates and records the namespace path variable.
func (h *handler) namespace(w http.ResponseWriter, r *http.Request, ctx surface.NamespaceErrorContext) (string, bool) {
	ns := mux.Vars(r)["namespace"]
	if err := namespace.ValidateName(ns); err != nil {
		surface.WriteNamespaceError(w, r, err, ctx)
		return "", false
	}
	observability.SetNamespace(r, ns)
	return ns, true
}

// spec resolves the namespace spec for hosted/proxy dispatch without
// authorizing — hosted routes authorize lazily through the guarded store.
func (h *handler) spec(w http.ResponseWriter, r *http.Request, ns string, write bool) (namespace.Spec, bool) {
	ac := auth.FromContext(r.Context())
	_, spec, err := h.reg.AuthorizedStore(r.Context(), ns, npmFormat, ac)
	if err != nil {
		surface.WriteNamespaceError(w, r, err, namespaceErrCtx(write))
		return namespace.Spec{}, false
	}
	return spec, true
}

// authorizedHostedStore returns the guarded store for a hosted namespace. Proxy
// namespaces are dispatched to the proxy engine before this is reached; the
// proxy guard here is a defensive safety net only.
func (h *handler) authorizedHostedStore(w http.ResponseWriter, r *http.Request, ns string, write bool) (core.Store, error) {
	ac := auth.FromContext(r.Context())
	store, spec, err := h.reg.AuthorizedStore(r.Context(), ns, npmFormat, ac)
	if err != nil {
		surface.WriteNamespaceError(w, r, err, namespaceErrCtx(write))
		return nil, err
	}
	if spec.IsProxy() {
		surface.WriteStoreError(w, r, core.ErrUnsupported)
		return nil, core.ErrUnsupported
	}
	return store, nil
}

// authorizedProxyStore authorizes the caller against the namespace reader policy
// and returns an unguarded store for pull-through cache fills.
func (h *handler) authorizedProxyStore(w http.ResponseWriter, r *http.Request, ns string) (core.Store, bool) {
	ac := auth.FromContext(r.Context())
	store, _, err := h.reg.AuthorizedProxyStore(r.Context(), ns, npmFormat, ac)
	if err != nil {
		surface.WriteNamespaceError(w, r, err, surface.NamespaceDataRead)
		return nil, false
	}
	return store, true
}

func namespaceErrCtx(write bool) surface.NamespaceErrorContext {
	if write {
		return surface.NamespaceDataWrite
	}
	return surface.NamespaceDataRead
}

// parsePackageVars assembles a PackageName from the router's scope/name vars.
func parsePackageVars(vars map[string]string) (PackageName, error) {
	if scope, ok := vars["scope"]; ok {
		return ParsePackageName("@" + scope + "/" + vars["name"])
	}
	return ParsePackageName(vars["name"])
}

func (h *handler) rootRoute(w http.ResponseWriter, r *http.Request) {
	observability.SetOperation(r, "root")
	ns, ok := h.namespace(w, r, surface.NamespaceDataRead)
	if !ok {
		return
	}
	if _, ok := h.spec(w, r, ns, false); !ok {
		return
	}
	surface.WriteJSON(w, http.StatusOK, map[string]any{"db_name": "open-artifact"})
}

func (h *handler) pingRoute(w http.ResponseWriter, r *http.Request) {
	observability.SetOperation(r, "ping")
	ns, ok := h.namespace(w, r, surface.NamespaceDataRead)
	if !ok {
		return
	}
	if _, ok := h.spec(w, r, ns, false); !ok {
		return
	}
	surface.WriteJSON(w, http.StatusOK, struct{}{})
}

func (h *handler) packageRoute(w http.ResponseWriter, r *http.Request) {
	pn, err := parsePackageVars(mux.Vars(r))
	if err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	write := r.Method == http.MethodPut
	if write {
		observability.SetOperation(r, "publish")
	} else {
		observability.SetOperation(r, "packument")
	}
	ns, ok := h.namespace(w, r, namespaceErrCtx(write))
	if !ok {
		return
	}
	spec, ok := h.spec(w, r, ns, write)
	if !ok {
		return
	}
	if spec.IsProxy() {
		if write {
			// A proxy namespace is a pull-through cache: it never accepts publishes.
			surface.WriteMethodNotAllowed(w, []string{http.MethodGet, http.MethodHead})
			return
		}
		store, ok := h.authorizedProxyStore(w, r, ns)
		if !ok {
			return
		}
		h.proxy.packument(w, r, ns, spec, store, pn)
		return
	}
	if write {
		h.publish(w, r, ns, pn)
		return
	}
	h.packument(w, r, ns, pn)
}

func (h *handler) tarballRoute(w http.ResponseWriter, r *http.Request) {
	observability.SetOperation(r, "download")
	pn, err := parsePackageVars(mux.Vars(r))
	if err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	filename := mux.Vars(r)["filename"]
	if err := ValidateTarballName(filename); err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	ns, ok := h.namespace(w, r, surface.NamespaceDataRead)
	if !ok {
		return
	}
	spec, ok := h.spec(w, r, ns, false)
	if !ok {
		return
	}
	if spec.IsProxy() {
		store, ok := h.authorizedProxyStore(w, r, ns)
		if !ok {
			return
		}
		h.proxy.tarball(w, r, ns, spec, store, pn, filename)
		return
	}
	store, err := h.authorizedHostedStore(w, r, ns, false)
	if err != nil {
		return
	}
	file, ok, err := findTarball(r.Context(), store.Package(pn.Core()), pn, filename)
	if err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	if !ok {
		surface.WriteStoreError(w, r, core.ErrNotFound)
		return
	}
	surface.RedirectOrStreamFile(w, r, file, "application/octet-stream")
}

func (h *handler) distTagsListRoute(w http.ResponseWriter, r *http.Request) {
	observability.SetOperation(r, "dist-tags.list")
	pn, err := parsePackageVars(mux.Vars(r))
	if err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	ns, ok := h.namespace(w, r, surface.NamespaceDataRead)
	if !ok {
		return
	}
	spec, ok := h.spec(w, r, ns, false)
	if !ok {
		return
	}
	if spec.IsProxy() {
		store, ok := h.authorizedProxyStore(w, r, ns)
		if !ok {
			return
		}
		h.proxy.distTags(w, r, ns, spec, store, pn)
		return
	}
	store, err := h.authorizedHostedStore(w, r, ns, false)
	if err != nil {
		return
	}
	pkg := store.Package(pn.Core())
	exists, err := pkg.Exists(r.Context())
	if err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	if !exists {
		surface.WriteStoreError(w, r, core.ErrNotFound)
		return
	}
	tags, err := pkg.TagTargets(r.Context())
	if err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	surface.WriteJSON(w, http.StatusOK, tags)
}

func (h *handler) distTagsRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		observability.SetOperation(r, "dist-tags.delete")
	} else {
		observability.SetOperation(r, "dist-tags.set")
	}
	pn, err := parsePackageVars(mux.Vars(r))
	if err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	tag := mux.Vars(r)["tag"]
	if err := ValidateTag(tag); err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	ns, ok := h.namespace(w, r, surface.NamespaceDataWrite)
	if !ok {
		return
	}
	spec, ok := h.spec(w, r, ns, true)
	if !ok {
		return
	}
	if spec.IsProxy() {
		// A proxy namespace is a pull-through cache: dist-tag add/delete (and any
		// other mutation) is rejected.
		surface.WriteMethodNotAllowed(w, []string{http.MethodGet, http.MethodHead})
		return
	}
	if r.Method == http.MethodDelete {
		// DELETE is not implemented in hosted mode in v1.
		surface.WriteStoreError(w, r, core.ErrUnsupported)
		return
	}
	store, err := h.authorizedHostedStore(w, r, ns, true)
	if err != nil {
		return
	}

	raw, tooLarge, err := surface.ReadCappedBody(w, r, 64<<10)
	if tooLarge {
		surface.WriteError(w, http.StatusRequestEntityTooLarge, "dist-tag body too large")
		return
	}
	if err != nil {
		surface.WriteError(w, http.StatusBadRequest, "invalid dist-tag body")
		return
	}
	version, err := parseTagVersion(raw)
	if err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}

	pkg := store.Package(pn.Core())
	exists, err := pkg.Version(version).Exists(r.Context())
	if err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	if !exists {
		surface.WriteStoreError(w, r, core.ErrNotFound)
		return
	}
	if err := pkg.SetTag(r.Context(), tag, version); err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	tags, err := pkg.TagTargets(r.Context())
	if err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	surface.WriteJSON(w, http.StatusOK, tags)
}

// parseTagVersion accepts either a JSON string ("\"1.2.3\"") or a bare version
// string, validates it, and returns the version.
func parseTagVersion(raw []byte) (string, error) {
	s := strings.TrimSpace(string(raw))
	var quoted string
	if err := json.Unmarshal(raw, &quoted); err == nil {
		s = strings.TrimSpace(quoted)
	}
	if err := ValidateVersion(s); err != nil {
		return "", err
	}
	return s, nil
}

func (h *handler) publish(w http.ResponseWriter, r *http.Request, ns string, pn PackageName) {
	store, err := h.authorizedHostedStore(w, r, ns, true)
	if err != nil {
		return
	}

	body, tooLarge, err := surface.ReadCappedBody(w, r, h.opts.uploadLimit())
	if tooLarge {
		surface.WriteError(w, http.StatusRequestEntityTooLarge, "upload too large")
		return
	}
	if err != nil {
		surface.WriteError(w, http.StatusBadRequest, "invalid publish body")
		return
	}

	var doc publishDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		surface.WriteError(w, http.StatusBadRequest, "invalid publish document")
		return
	}
	if doc.Name != pn.Original {
		surface.WriteError(w, http.StatusBadRequest, "publish name does not match URL")
		return
	}
	if len(doc.Versions) != 1 {
		surface.WriteError(w, http.StatusBadRequest, "publish must contain exactly one version")
		return
	}

	var version string
	var rawMeta json.RawMessage
	for v, m := range doc.Versions {
		version, rawMeta = v, m
	}
	if err := ValidateVersion(version); err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}

	meta := map[string]any{}
	if err := json.Unmarshal(rawMeta, &meta); err != nil {
		surface.WriteError(w, http.StatusBadRequest, "invalid version metadata")
		return
	}
	dist, _ := meta["dist"].(map[string]any)
	tarballName, err := tarballStorageName(pn, version, dist)
	if err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	att, ok := pickAttachment(doc.Attachments, tarballName, pn, version)
	if !ok {
		surface.WriteError(w, http.StatusBadRequest, "missing tarball attachment")
		return
	}
	expected, err := expectedDigests(dist)
	if err != nil {
		surface.WriteError(w, http.StatusBadRequest, "invalid integrity declaration")
		return
	}

	// Rewrite the tarball URL to point back at this registry before persisting
	// the version metadata, so the stored document never references npmjs.org
	// or a publisher-provided host.
	if dist == nil {
		dist = map[string]any{}
		meta["dist"] = dist
	}
	dist["tarball"] = tarballURL(requestBaseURL(r), ns, pn, tarballName)
	rewritten, err := json.Marshal(meta)
	if err != nil {
		surface.WriteError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	// Stream the base64 attachment straight into the Store rather than holding
	// the decoded tarball in memory; the Store hashes it during the write and
	// verifies the declared SHA-1/SHA-512 before committing. This is also the
	// immutability gate: a republish of an existing version collides as 409,
	// and a corrupt upload aborts without leaving a servable blob.
	uploadedAt := h.now().UTC().Format(time.RFC3339Nano)
	pkg := store.Package(pn.Core())
	ver := pkg.Version(version)
	tarballBody := base64.NewDecoder(base64.StdEncoding, strings.NewReader(strings.TrimSpace(att.Data)))
	if _, err := ver.AddFile(r.Context(), tarballName, tarballBody,
		core.WithExpectedDigests(expected...),
		core.WithAnnotations(map[string]any{
			"npm:name":        pn.Original,
			"npm:version":     version,
			"npm:filename":    tarballName,
			"npm:uploaded_at": uploadedAt,
		}),
	); err != nil {
		var corrupt base64.CorruptInputError
		if errors.As(err, &corrupt) {
			surface.WriteError(w, http.StatusBadRequest, "invalid base64 tarball attachment")
			return
		}
		surface.WriteStoreError(w, r, err)
		return
	}

	// Record the package/version envelopes. Ordering after the tarball keeps a
	// rejected upload from leaving any package or version metadata behind.
	if _, err := store.AddPackage(r.Context(), pn.Core(), core.WithAnnotations(map[string]any{
		"npm:name": pn.Original,
	})); err != nil && !errors.Is(err, core.ErrAlreadyExists) {
		surface.WriteStoreError(w, r, err)
		return
	}
	if _, err := pkg.AddVersion(r.Context(), version, core.WithAnnotations(map[string]any{
		"npm:name":        pn.Original,
		"npm:version":     version,
		"npm:uploaded_at": uploadedAt,
	})); err != nil && !errors.Is(err, core.ErrAlreadyExists) {
		surface.WriteStoreError(w, r, err)
		return
	}
	if _, err := ver.AddFile(r.Context(), metadataFile, bytes.NewReader(rewritten), core.WithAllowOverwrite(true)); err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}

	if err := h.applyDistTags(r, pkg, doc.DistTags, version); err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}

	surface.WriteJSON(w, http.StatusCreated, map[string]any{"ok": true, "id": pn.Original, "rev": ""})
}

// applyDistTags applies the request's dist-tags, defaulting "latest" to the
// published version when the client sent none (npm relies on this).
func (h *handler) applyDistTags(r *http.Request, pkg core.Package, tags map[string]string, version string) error {
	if len(tags) == 0 {
		return pkg.SetTag(r.Context(), "latest", version)
	}
	names := make([]string, 0, len(tags))
	for name := range tags {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := ValidateTag(name); err != nil {
			return err
		}
		if err := ValidateVersion(tags[name]); err != nil {
			return err
		}
		if err := pkg.SetTag(r.Context(), name, tags[name]); err != nil {
			return err
		}
	}
	return nil
}

func (h *handler) packument(w http.ResponseWriter, r *http.Request, ns string, pn PackageName) {
	store, err := h.authorizedHostedStore(w, r, ns, false)
	if err != nil {
		return
	}
	pkg := store.Package(pn.Core())
	exists, err := pkg.Exists(r.Context())
	if err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	if !exists {
		surface.WriteStoreError(w, r, core.ErrNotFound)
		return
	}

	versions, err := pkg.Versions(r.Context())
	if err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}

	base := requestBaseURL(r)
	doc := Packument{
		ID:       pn.Original,
		Name:     pn.Original,
		DistTags: map[string]string{},
		Versions: map[string]any{},
		Time:     map[string]string{},
	}

	// Each version's metadata is an independent pair of reads (package.json plus
	// the version envelope for its timestamp), so fan them out concurrently
	// (bounded) and merge under a mutex. The resulting maps are key-ordered by
	// JSON marshaling, so output stays deterministic regardless of completion
	// order.
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		firstErr error
	)
	sem := make(chan struct{}, versionReadConcurrency)
	for _, version := range versions {
		version := version
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			meta, uploadedAt, err := readVersionMetadata(r.Context(), version)
			if err != nil {
				// A version directory without package.json is a partial write;
				// skip it rather than failing the whole packument.
				if errors.Is(err, core.ErrNotFound) {
					return
				}
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}
			rewriteTarball(meta, base, ns, pn)
			mu.Lock()
			doc.Versions[version.Name()] = meta
			if uploadedAt != "" {
				doc.Time[version.Name()] = uploadedAt
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if firstErr != nil {
		surface.WriteStoreError(w, r, firstErr)
		return
	}
	if len(doc.Versions) == 0 {
		surface.WriteStoreError(w, r, core.ErrNotFound)
		return
	}
	tags, err := pkg.TagTargets(r.Context())
	if err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	doc.DistTags = tags
	if len(doc.Time) == 0 {
		doc.Time = nil
	}

	if r.Method == http.MethodHead {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		return
	}
	surface.WriteJSON(w, http.StatusOK, doc)
}

// readVersionMetadata reads a version's stored package.json document and its
// upload timestamp annotation.
func readVersionMetadata(ctx context.Context, version core.Version) (map[string]any, string, error) {
	rc, err := version.File(metadataFile).Read(ctx)
	if err != nil {
		return nil, "", err
	}
	defer rc.Close()
	raw, err := io.ReadAll(rc)
	if err != nil {
		return nil, "", err
	}
	meta := map[string]any{}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, "", err
	}
	uploadedAt := ""
	if m, err := version.Meta(ctx); err == nil {
		uploadedAt, _ = m.Annotations["npm:uploaded_at"].(string)
	}
	return meta, uploadedAt, nil
}

// rewriteTarball points the version document's dist.tarball at this registry,
// using the request's host so links are valid regardless of where the metadata
// was published.
func rewriteTarball(meta map[string]any, base, ns string, pn PackageName) {
	dist, ok := meta["dist"].(map[string]any)
	if !ok {
		return
	}
	current, _ := dist["tarball"].(string)
	filename := path.Base(current)
	if filename == "" || filename == "." || filename == "/" {
		return
	}
	dist["tarball"] = tarballURL(base, ns, pn, filename)
}

// findTarball locates the File holding filename. The npm convention names a
// tarball "<unscoped>-<version>.tgz", so the version is derived first for a
// direct lookup; if that misses (unusual filenames), every version is scanned.
func findTarball(ctx context.Context, pkg core.Package, pn PackageName, filename string) (core.File, bool, error) {
	if version := deriveVersion(pn, filename); version != "" {
		file := pkg.Version(version).File(filename)
		exists, err := file.Exists(ctx)
		if err != nil {
			return nil, false, err
		}
		if exists {
			return file, true, nil
		}
	}
	versions, err := pkg.Versions(ctx)
	if err != nil {
		return nil, false, err
	}
	for _, version := range versions {
		file := version.File(filename)
		exists, err := file.Exists(ctx)
		if err != nil {
			return nil, false, err
		}
		if exists {
			return file, true, nil
		}
	}
	return nil, false, nil
}

// deriveVersion strips the "<unscoped>-" prefix and ".tgz" suffix npm uses for
// tarball filenames, yielding the version. It returns "" when the filename does
// not follow that convention.
func deriveVersion(pn PackageName, filename string) string {
	trimmed := strings.TrimSuffix(filename, ".tgz")
	prefix := pn.Unscoped() + "-"
	if !strings.HasPrefix(trimmed, prefix) {
		return ""
	}
	return strings.TrimPrefix(trimmed, prefix)
}

// tarballStorageName is the filename the tarball is stored and served under.
// npm names it after the unscoped package ("<unscoped>-<version>.tgz"), which
// is also the basename of dist.tarball, so the download route can derive the
// version from it. It prefers the dist.tarball basename and falls back to the
// convention when dist is absent.
func tarballStorageName(pn PackageName, version string, dist map[string]any) (string, error) {
	if dist != nil {
		if t, _ := dist["tarball"].(string); t != "" {
			name := path.Base(t)
			if err := ValidateTarballName(name); err != nil {
				return "", err
			}
			return name, nil
		}
	}
	return pn.Unscoped() + "-" + version + ".tgz", nil
}

// pickAttachment finds the published tarball bytes. A single-version publish
// carries exactly one attachment, so that is preferred; otherwise it is looked
// up by the storage name or by npm's full-name key ("<name>-<version>.tgz",
// which for a scoped package differs from the unscoped storage name).
func pickAttachment(attachments map[string]attachment, storageName string, pn PackageName, version string) (attachment, bool) {
	if len(attachments) == 1 {
		for _, a := range attachments {
			return a, true
		}
	}
	if a, ok := attachments[storageName]; ok {
		return a, true
	}
	if a, ok := attachments[pn.Original+"-"+version+".tgz"]; ok {
		return a, true
	}
	return attachment{}, false
}

// requestBaseURL builds the scheme://host prefix for absolute tarball URLs,
// honoring X-Forwarded-Proto/Host so links are correct behind a TLS-terminating
// proxy. Operators must ensure those headers are set by a trusted front end.
func requestBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = p
	}
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = h
	}
	return scheme + "://" + host
}

// tarballURL builds the registry-rooted download URL for a tarball.
func tarballURL(base, ns string, pn PackageName, filename string) string {
	p := "/" + url.PathEscape(ns)
	if pn.Scoped() {
		p += "/@" + url.PathEscape(pn.Scope) + "/" + url.PathEscape(pn.Name)
	} else {
		p += "/" + url.PathEscape(pn.Name)
	}
	p += "/-/" + url.PathEscape(filename)
	return base + p
}
