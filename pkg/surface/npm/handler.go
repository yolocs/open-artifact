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

type Config struct {
	MaxUploadBytes int64
}

func (c Config) uploadLimit() int64 {
	if c.MaxUploadBytes <= 0 {
		return DefaultMaxUploadBytes
	}
	return c.MaxUploadBytes
}

// Handler builds the npm registry surface. It composes namespace lookup and
// authorization, hosted/proxy dispatch, and the shared error/redirect helpers
// exactly like the PyPI surface; only the wire protocol and codec are
// npm-specific.
func Handler(reg *namespace.Registry, authn auth.Authenticator, cfg Config) http.Handler {
	h := &handler{reg: reg, opts: cfg, now: time.Now}
	return auth.Middleware(authn)(h.router())
}

type handler struct {
	reg  *namespace.Registry
	opts Config
	now  func() time.Time
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

// authorizedHostedStore returns the guarded store for a hosted namespace,
// writing the appropriate error and rejecting proxy namespaces.
func (h *handler) authorizedHostedStore(w http.ResponseWriter, r *http.Request, ns string, write bool) (core.Store, error) {
	ac := auth.FromContext(r.Context())
	store, spec, err := h.reg.AuthorizedStore(r.Context(), ns, npmFormat, ac)
	if err != nil {
		surface.WriteNamespaceError(w, r, err, namespaceErrCtx(write))
		return nil, err
	}
	if spec.IsProxy() {
		// Proxy mode is implemented separately (#22). Until then writes are
		// rejected and reads are not implemented.
		surface.WriteStoreError(w, r, core.ErrUnsupported)
		return nil, core.ErrUnsupported
	}
	return store, nil
}

func namespaceErrCtx(write bool) surface.NamespaceErrorContext {
	if write {
		return surface.NamespaceDataWrite
	}
	return surface.NamespaceDataRead
}

// rejectProxy handles a proxy-mode namespace: a write is rejected with 405
// (Allow: GET, HEAD) since a proxy never accepts uploads, and a read maps to
// ErrUnsupported (501) until the npm proxy lands (#22). It returns true when it
// wrote a response.
func (h *handler) rejectProxy(w http.ResponseWriter, r *http.Request, spec namespace.Spec, write bool) bool {
	if !spec.IsProxy() {
		return false
	}
	if write {
		surface.WriteMethodNotAllowed(w, []string{http.MethodGet, http.MethodHead})
	} else {
		surface.WriteStoreError(w, r, core.ErrUnsupported)
	}
	return true
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
	if h.rejectProxy(w, r, spec, write) {
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
	if h.rejectProxy(w, r, spec, false) {
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
	if h.rejectProxy(w, r, spec, false) {
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
	tags, err := readDistTags(r.Context(), pkg)
	if err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	surface.WriteJSON(w, http.StatusOK, tags)
}

func (h *handler) distTagsRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		observability.SetOperation(r, "dist-tags.delete")
		surface.WriteStoreError(w, r, core.ErrUnsupported)
		return
	}
	observability.SetOperation(r, "dist-tags.set")
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
	if h.rejectProxy(w, r, spec, true) {
		return
	}
	store, err := h.authorizedHostedStore(w, r, ns, true)
	if err != nil {
		return
	}

	r = surface.WithMaxBody(w, r, 64<<10)
	raw, err := io.ReadAll(r.Body)
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
	tags, err := readDistTags(r.Context(), pkg)
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

	r = surface.WithMaxBody(w, r, h.opts.uploadLimit())
	body, err := io.ReadAll(r.Body)
	if err != nil {
		if strings.Contains(err.Error(), "request body too large") {
			surface.WriteError(w, http.StatusRequestEntityTooLarge, "upload too large")
			return
		}
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
	tarballName, err := resolveTarballName(pn, version, dist, doc.Attachments)
	if err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	att, ok := doc.Attachments[tarballName]
	if !ok {
		surface.WriteError(w, http.StatusBadRequest, "missing tarball attachment")
		return
	}
	tarball, err := base64.StdEncoding.DecodeString(strings.TrimSpace(att.Data))
	if err != nil {
		surface.WriteError(w, http.StatusBadRequest, "invalid base64 tarball attachment")
		return
	}

	declaredShasum, declaredIntegrity := "", ""
	if dist != nil {
		declaredShasum, _ = dist["shasum"].(string)
		declaredIntegrity, _ = dist["integrity"].(string)
	}
	if err := verifyIntegrity(tarball, declaredShasum, declaredIntegrity); err != nil {
		surface.WriteStoreError(w, r, err)
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

	uploadedAt := h.now().UTC().Format(time.RFC3339Nano)
	if _, err := store.AddPackage(r.Context(), pn.Core(), core.WithAnnotations(map[string]any{
		"npm:name": pn.Original,
	})); err != nil && !errors.Is(err, core.ErrAlreadyExists) {
		surface.WriteStoreError(w, r, err)
		return
	}
	pkg := store.Package(pn.Core())
	if _, err := pkg.AddVersion(r.Context(), version, core.WithAnnotations(map[string]any{
		"npm:name":        pn.Original,
		"npm:version":     version,
		"npm:uploaded_at": uploadedAt,
	})); err != nil && !errors.Is(err, core.ErrAlreadyExists) {
		surface.WriteStoreError(w, r, err)
		return
	}

	ver := pkg.Version(version)
	// The tarball write gates immutability: a republish of an existing version
	// collides here and surfaces as 409.
	if _, err := ver.AddFile(r.Context(), tarballName, bytes.NewReader(tarball), core.WithAnnotations(map[string]any{
		"npm:name":        pn.Original,
		"npm:version":     version,
		"npm:filename":    tarballName,
		"npm:shasum":      sha1Hex(tarball),
		"npm:integrity":   sha512SRI(tarball),
		"npm:length":      len(tarball),
		"npm:uploaded_at": uploadedAt,
	})); err != nil {
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
	for _, version := range versions {
		meta, uploadedAt, err := readVersionMetadata(r.Context(), version)
		if err != nil {
			if errors.Is(err, core.ErrNotFound) {
				// A version directory without package.json is a partial write;
				// skip it rather than failing the whole packument.
				continue
			}
			surface.WriteStoreError(w, r, err)
			return
		}
		rewriteTarball(meta, base, ns, pn)
		doc.Versions[version.Name()] = meta
		if uploadedAt != "" {
			doc.Time[version.Name()] = uploadedAt
		}
	}
	if len(doc.Versions) == 0 {
		surface.WriteStoreError(w, r, core.ErrNotFound)
		return
	}
	tags, err := readDistTags(r.Context(), pkg)
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

// readDistTags resolves every dist-tag on the package to its target version.
func readDistTags(ctx context.Context, pkg core.Package) (map[string]string, error) {
	tags, err := pkg.Tags(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(tags))
	for _, t := range tags {
		ref, err := t.Ref(ctx)
		if err != nil {
			return nil, err
		}
		out[t.Name()] = ref.Name()
	}
	return out, nil
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

// resolveTarballName determines the attachment filename to store under: the
// basename of dist.tarball when present, otherwise the sole attachment key.
func resolveTarballName(pn PackageName, version string, dist map[string]any, attachments map[string]attachment) (string, error) {
	if dist != nil {
		if t, _ := dist["tarball"].(string); t != "" {
			name := path.Base(t)
			if err := ValidateTarballName(name); err != nil {
				return "", err
			}
			return name, nil
		}
	}
	if len(attachments) == 1 {
		for name := range attachments {
			if err := ValidateTarballName(name); err != nil {
				return "", err
			}
			return name, nil
		}
	}
	return "", core.ErrNotFound
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
