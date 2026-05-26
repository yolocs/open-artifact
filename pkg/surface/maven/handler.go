package maven

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/yolocs/open-artifact/pkg/auth"
	"github.com/yolocs/open-artifact/pkg/core"
	"github.com/yolocs/open-artifact/pkg/namespace"
	"github.com/yolocs/open-artifact/pkg/observability"
	"github.com/yolocs/open-artifact/pkg/surface"
)

const DefaultMaxUploadBytes int64 = 100 << 20

// Proxy-mode defaults. Maven metadata is small and fetched live, so the
// upstream body cap only needs to bound a pathological metadata document; the
// negative TTL bounds how long an upstream 404 (artifact or metadata) is
// remembered. Artifact bytes are streamed straight through (tee-to-store), so
// there is no artifact buffer to cap.
const (
	DefaultProxyMetadataMaxBytes int64 = 32 << 20
	DefaultProxyNegativeCacheTTL       = 30 * time.Second
)

type Config struct {
	MaxUploadBytes int64
	AllowOverwrite bool

	// Proxy-mode knobs. A zero value falls back to the matching Default above.
	ProxyMetadataMaxBytes int64
	ProxyNegativeCacheTTL time.Duration
}

func (c Config) uploadLimit() int64 {
	if c.MaxUploadBytes <= 0 {
		return DefaultMaxUploadBytes
	}
	return c.MaxUploadBytes
}

func (c Config) proxyMetadataMaxBytes() int64 {
	if c.ProxyMetadataMaxBytes <= 0 {
		return DefaultProxyMetadataMaxBytes
	}
	return c.ProxyMetadataMaxBytes
}

func Handler(reg *namespace.Registry, authn auth.Authenticator, cfg Config) http.Handler {
	now := time.Now
	h := &handler{reg: reg, opts: cfg, now: now, proxy: newProxyEngine(cfg, now)}
	return auth.Middleware(authn)(http.HandlerFunc(h.serveHTTP))
}

type handler struct {
	reg   *namespace.Registry
	opts  Config
	now   func() time.Time
	proxy *proxyEngine
}

func (h *handler) serveHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		h.download(w, r)
	case http.MethodPut, http.MethodPost:
		h.upload(w, r)
	default:
		surface.WriteMethodNotAllowed(w, []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut})
	}
}

func (h *handler) upload(w http.ResponseWriter, r *http.Request) {
	observability.SetOperation(r, "upload")
	p, err := parsePath(r.URL.EscapedPath())
	if err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	observability.SetNamespace(r, p.Namespace)
	store, spec, err := h.authorizedStore(w, r, p.Namespace, true)
	if err != nil {
		return
	}
	if spec.IsProxy() {
		surface.WriteMethodNotAllowed(w, []string{http.MethodGet, http.MethodHead})
		return
	}

	body, ok := h.readUploadBody(w, r)
	if !ok {
		return
	}
	if p.Checksum != checksumNone {
		if err := h.verifyChecksumUpload(r, store, p, body); err != nil {
			if errors.Is(err, core.ErrNotFound) {
				surface.WriteError(w, http.StatusConflict, "checksum target not found")
				return
			}
			surface.WriteStoreError(w, r, err)
			return
		}
	}
	if err := h.ensureParents(r, store, p); err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	opts := []core.CreateOption{core.WithAnnotations(uploadAnnotations(p, h.now().UTC()))}
	if h.allowOverwrite(p) {
		opts = append(opts, core.WithAllowOverwrite(true))
	}
	if _, err := h.addFile(r, store, p, bytes.NewReader(body), opts...); err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusCreated)
	_, _ = io.WriteString(w, "uploaded\n")
}

func (h *handler) download(w http.ResponseWriter, r *http.Request) {
	observability.SetOperation(r, "download")
	p, err := parsePath(r.URL.EscapedPath())
	if err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	observability.SetNamespace(r, p.Namespace)
	store, spec, err := h.authorizedStore(w, r, p.Namespace, false)
	if err != nil {
		return
	}
	if spec.IsProxy() {
		pstore, ok := h.authorizedProxyStore(w, r, p.Namespace)
		if !ok {
			return
		}
		h.proxy.serve(w, r, p, p.Namespace, spec, pstore)
		return
	}
	surface.RedirectOrStreamFile(w, r, h.file(store, p, p.File), contentType(p))
}

// authorizedProxyStore authorizes the caller against the namespace reader policy
// and returns an unguarded store for pull-through cache fills.
func (h *handler) authorizedProxyStore(w http.ResponseWriter, r *http.Request, ns string) (core.Store, bool) {
	ac := auth.FromContext(r.Context())
	store, _, err := h.reg.AuthorizedProxyStore(r.Context(), ns, mavenFormat, ac)
	if err != nil {
		surface.WriteNamespaceError(w, r, err, surface.NamespaceDataRead)
		return nil, false
	}
	return store, true
}

func (h *handler) authorizedStore(w http.ResponseWriter, r *http.Request, ns string, write bool) (core.Store, namespace.Spec, error) {
	ac := auth.FromContext(r.Context())
	store, spec, err := h.reg.AuthorizedStore(r.Context(), ns, mavenFormat, ac)
	if err != nil {
		ctx := surface.NamespaceDataRead
		if write {
			ctx = surface.NamespaceDataWrite
		}
		surface.WriteNamespaceError(w, r, err, ctx)
		return nil, namespace.Spec{}, err
	}
	return store, spec, nil
}

func (h *handler) readUploadBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	limited := surface.WithMaxBody(w, r, h.opts.uploadLimit())
	body, err := io.ReadAll(limited.Body)
	if err != nil {
		if strings.Contains(err.Error(), "request body too large") {
			surface.WriteError(w, http.StatusRequestEntityTooLarge, "upload too large")
			return nil, false
		}
		surface.WriteError(w, http.StatusBadRequest, "invalid upload body")
		return nil, false
	}
	return body, true
}

func (h *handler) verifyChecksumUpload(r *http.Request, store core.Store, p requestPath, body []byte) error {
	rc, err := h.file(store, p, p.TargetFile).Read(r.Context())
	if err != nil {
		return err
	}
	err = verifyChecksum(p.Checksum, rc, bytes.NewReader(body))
	closeErr := rc.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func (h *handler) ensureParents(r *http.Request, store core.Store, p requestPath) error {
	if p.Kind == pathArchetypeCatalog {
		return nil
	}
	if _, err := store.AddPackage(r.Context(), p.Package, core.WithAnnotations(map[string]any{
		"maven:group_id":    p.GroupID,
		"maven:artifact_id": p.ArtifactID,
	})); err != nil && !errors.Is(err, core.ErrAlreadyExists) {
		return err
	}
	if p.Kind == pathArtifactMetadata {
		return nil
	}
	if _, err := store.Package(p.Package).AddVersion(r.Context(), p.Version, core.WithAnnotations(map[string]any{
		"maven:version": p.Version,
	})); err != nil && !errors.Is(err, core.ErrAlreadyExists) {
		return err
	}
	return nil
}

func (h *handler) addFile(r *http.Request, store core.Store, p requestPath, body io.Reader, opts ...core.CreateOption) (core.File, error) {
	switch p.Kind {
	case pathArchetypeCatalog:
		return store.AddFile(r.Context(), p.File, body, opts...)
	case pathArtifactMetadata:
		return store.Package(p.Package).AddFile(r.Context(), p.File, body, opts...)
	default:
		return store.Package(p.Package).Version(p.Version).AddFile(r.Context(), p.File, body, opts...)
	}
}

func (h *handler) file(store core.Store, p requestPath, name string) core.File {
	switch p.Kind {
	case pathArchetypeCatalog:
		return store.File(name)
	case pathArtifactMetadata:
		return store.Package(p.Package).File(name)
	default:
		return store.Package(p.Package).Version(p.Version).File(name)
	}
}

func (h *handler) allowOverwrite(p requestPath) bool {
	return h.opts.AllowOverwrite || isSnapshotVersion(p.Version)
}

func uploadAnnotations(p requestPath, uploadedAt time.Time) map[string]any {
	out := map[string]any{
		"maven:file":        p.File,
		"maven:uploaded_at": uploadedAt.Format(time.RFC3339Nano),
	}
	if p.GroupID != "" {
		out["maven:group_id"] = p.GroupID
	}
	if p.ArtifactID != "" {
		out["maven:artifact_id"] = p.ArtifactID
	}
	if p.Checksum != checksumNone {
		out["maven:checksum"] = string(p.Checksum)
		out["maven:checksum_target"] = p.TargetFile
	}
	return out
}

func contentType(p requestPath) string {
	if p.File == metadataFile || p.File == archetypeFile {
		return "application/xml"
	}
	return "application/octet-stream"
}
