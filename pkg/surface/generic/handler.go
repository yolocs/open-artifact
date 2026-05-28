package generic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/yolocs/open-artifact/pkg/auth"
	"github.com/yolocs/open-artifact/pkg/core"
	"github.com/yolocs/open-artifact/pkg/namespace"
	"github.com/yolocs/open-artifact/pkg/observability"
	"github.com/yolocs/open-artifact/pkg/surface"
)

// DefaultMaxUploadBytes caps a single file upload when --generic-max-upload-bytes
// is unset, matching the other hosted formats.
const DefaultMaxUploadBytes int64 = 100 << 20

// createBodyLimit caps the optional annotations body on a package/version PUT.
// It is metadata, not artifact content, so the cap is small.
const createBodyLimit int64 = 1 << 20

type Config struct {
	MaxUploadBytes int64
	AllowOverwrite bool
}

func (c Config) uploadLimit() int64 {
	if c.MaxUploadBytes <= 0 {
		return DefaultMaxUploadBytes
	}
	return c.MaxUploadBytes
}

// Handler builds the generic REST surface. Every route is wrapped with the auth
// middleware; per-namespace authorization is enforced below the surface by the
// guarded Store. DELETE is intentionally unregistered (deferred), so the mux
// answers it with 405 and an Allow header.
func Handler(reg *namespace.Registry, authn auth.Authenticator, cfg Config) http.Handler {
	h := &handler{reg: reg, cfg: cfg}
	mux := http.NewServeMux()
	const root = "/{namespace}/generic"
	mux.HandleFunc("GET "+root+"/packages", h.listPackages)
	mux.HandleFunc("GET "+root+"/packages/{package}", h.getPackage)
	mux.HandleFunc("PUT "+root+"/packages/{package}", h.putPackage)
	mux.HandleFunc("GET "+root+"/packages/{package}/versions", h.listVersions)
	mux.HandleFunc("GET "+root+"/packages/{package}/versions/{version}", h.getVersion)
	mux.HandleFunc("PUT "+root+"/packages/{package}/versions/{version}", h.putVersion)
	mux.HandleFunc("GET "+root+"/packages/{package}/versions/{version}/files", h.listFiles)
	mux.HandleFunc("GET "+root+"/packages/{package}/versions/{version}/files/{file}", h.downloadFile)
	mux.HandleFunc("PUT "+root+"/packages/{package}/versions/{version}/files/{file}", h.uploadFile)
	return auth.Middleware(authn)(mux)
}

type handler struct {
	reg *namespace.Registry
	cfg Config
}

// store resolves the namespace-scoped, authorized Store and rejects proxy-mode
// namespaces (generic is hosted-only in v1). On any failure it writes the
// response and returns ok=false. Per-operation authorization happens inside the
// returned Store's guard.
func (h *handler) store(w http.ResponseWriter, r *http.Request, ns string, write bool) (core.Store, bool) {
	observability.SetNamespace(r, ns)
	ac := auth.FromContext(r.Context())
	store, spec, err := h.reg.AuthorizedStore(r.Context(), ns, genericFormat, ac)
	if err != nil {
		ctx := surface.NamespaceDataRead
		if write {
			ctx = surface.NamespaceDataWrite
		}
		surface.WriteNamespaceError(w, r, err, ctx)
		return nil, false
	}
	if spec.IsProxy() {
		surface.WriteError(w, http.StatusNotImplemented, "generic proxy mode is not supported")
		return nil, false
	}
	return store, true
}

func (h *handler) listPackages(w http.ResponseWriter, r *http.Request) {
	observability.SetOperation(r, "list")
	ns := r.PathValue("namespace")
	store, ok := h.store(w, r, ns, false)
	if !ok {
		return
	}
	pkgs, err := store.Packages(r.Context())
	if err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	surface.WriteJSON(w, http.StatusOK, packageListResponse{Namespace: ns, Packages: names(pkgs)})
}

func (h *handler) getPackage(w http.ResponseWriter, r *http.Request) {
	observability.SetOperation(r, "read")
	ns := r.PathValue("namespace")
	store, ok := h.store(w, r, ns, false)
	if !ok {
		return
	}
	name := r.PathValue("package")
	pkg := store.Package(name)
	exists, err := pkg.Exists(r.Context())
	if err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	if !exists {
		surface.WriteError(w, http.StatusNotFound, "package not found")
		return
	}
	versions, err := pkg.Versions(r.Context())
	if err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	meta, err := pkg.Meta(r.Context())
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		surface.WriteStoreError(w, r, err)
		return
	}
	surface.WriteJSON(w, http.StatusOK, packageResponse{
		Name:        name,
		CreatedAt:   timePtr(meta.CreatedAt),
		UpdatedAt:   timePtr(meta.UpdatedAt),
		Annotations: meta.Annotations,
		Versions:    names(versions),
	})
}

func (h *handler) putPackage(w http.ResponseWriter, r *http.Request) {
	observability.SetOperation(r, "create")
	ns := r.PathValue("namespace")
	store, ok := h.store(w, r, ns, true)
	if !ok {
		return
	}
	ann, ok := h.readCreate(w, r)
	if !ok {
		return
	}
	name := r.PathValue("package")
	status, err := upsertContainer(r.Context(), store.Package(name), func(opts ...core.CreateOption) error {
		_, err := store.AddPackage(r.Context(), name, opts...)
		return err
	}, ann)
	if err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	surface.WriteJSON(w, status, map[string]string{"name": name})
}

func (h *handler) listVersions(w http.ResponseWriter, r *http.Request) {
	observability.SetOperation(r, "list")
	ns := r.PathValue("namespace")
	store, ok := h.store(w, r, ns, false)
	if !ok {
		return
	}
	name := r.PathValue("package")
	versions, err := store.Package(name).Versions(r.Context())
	if err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	surface.WriteJSON(w, http.StatusOK, versionListResponse{Namespace: ns, Package: name, Versions: names(versions)})
}

func (h *handler) getVersion(w http.ResponseWriter, r *http.Request) {
	observability.SetOperation(r, "read")
	ns := r.PathValue("namespace")
	store, ok := h.store(w, r, ns, false)
	if !ok {
		return
	}
	pkgName := r.PathValue("package")
	verName := r.PathValue("version")
	ver := store.Package(pkgName).Version(verName)
	exists, err := ver.Exists(r.Context())
	if err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	if !exists {
		surface.WriteError(w, http.StatusNotFound, "version not found")
		return
	}
	files, err := ver.Files(r.Context())
	if err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	meta, err := ver.Meta(r.Context())
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		surface.WriteStoreError(w, r, err)
		return
	}
	surface.WriteJSON(w, http.StatusOK, versionResponse{
		Package:     pkgName,
		Version:     verName,
		CreatedAt:   timePtr(meta.CreatedAt),
		UpdatedAt:   timePtr(meta.UpdatedAt),
		Annotations: meta.Annotations,
		Files:       names(files),
	})
}

func (h *handler) putVersion(w http.ResponseWriter, r *http.Request) {
	observability.SetOperation(r, "create")
	ns := r.PathValue("namespace")
	store, ok := h.store(w, r, ns, true)
	if !ok {
		return
	}
	ann, ok := h.readCreate(w, r)
	if !ok {
		return
	}
	pkgName := r.PathValue("package")
	verName := r.PathValue("version")
	if _, err := store.AddPackage(r.Context(), pkgName); err != nil && !errors.Is(err, core.ErrAlreadyExists) {
		surface.WriteStoreError(w, r, err)
		return
	}
	pkg := store.Package(pkgName)
	status, err := upsertContainer(r.Context(), pkg.Version(verName), func(opts ...core.CreateOption) error {
		_, err := pkg.AddVersion(r.Context(), verName, opts...)
		return err
	}, ann)
	if err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	surface.WriteJSON(w, status, map[string]string{"package": pkgName, "version": verName})
}

func (h *handler) listFiles(w http.ResponseWriter, r *http.Request) {
	observability.SetOperation(r, "list")
	ns := r.PathValue("namespace")
	store, ok := h.store(w, r, ns, false)
	if !ok {
		return
	}
	pkgName := r.PathValue("package")
	verName := r.PathValue("version")
	files, err := store.Package(pkgName).Version(verName).Files(r.Context())
	if err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	out := make([]fileResponse, 0, len(files))
	for _, f := range files {
		m, err := f.Meta(r.Context())
		if err != nil {
			surface.WriteStoreError(w, r, err)
			return
		}
		out = append(out, fileToResponse(f.Name(), m))
	}
	surface.WriteJSON(w, http.StatusOK, fileListResponse{Namespace: ns, Package: pkgName, Version: verName, Files: out})
}

func (h *handler) downloadFile(w http.ResponseWriter, r *http.Request) {
	observability.SetOperation(r, "download")
	ns := r.PathValue("namespace")
	store, ok := h.store(w, r, ns, false)
	if !ok {
		return
	}
	f := store.Package(r.PathValue("package")).Version(r.PathValue("version")).File(r.PathValue("file"))
	meta, err := f.Meta(r.Context())
	if err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	surface.RedirectOrStreamFile(w, r, f, downloadContentType(f.Name(), meta))
}

func (h *handler) uploadFile(w http.ResponseWriter, r *http.Request) {
	observability.SetOperation(r, "upload")
	ns := r.PathValue("namespace")
	store, ok := h.store(w, r, ns, true)
	if !ok {
		return
	}
	expected, err := parseChecksumHeaders(r.Header)
	if err != nil {
		surface.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	pkgName := r.PathValue("package")
	verName := r.PathValue("version")
	fileName := r.PathValue("file")
	if _, err := store.AddPackage(r.Context(), pkgName); err != nil && !errors.Is(err, core.ErrAlreadyExists) {
		surface.WriteStoreError(w, r, err)
		return
	}
	pkg := store.Package(pkgName)
	if _, err := pkg.AddVersion(r.Context(), verName); err != nil && !errors.Is(err, core.ErrAlreadyExists) {
		surface.WriteStoreError(w, r, err)
		return
	}

	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	opts := []core.CreateOption{core.WithAnnotations(map[string]any{annContentType: contentType})}
	if len(expected) > 0 {
		opts = append(opts, core.WithExpectedDigests(expected...))
	}
	if h.cfg.AllowOverwrite {
		opts = append(opts, core.WithAllowOverwrite(true))
	}

	limited := surface.WithMaxBody(w, r, h.cfg.uploadLimit())
	f, err := pkg.Version(verName).AddFile(r.Context(), fileName, limited.Body, opts...)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			surface.WriteError(w, http.StatusRequestEntityTooLarge, "upload too large")
			return
		}
		surface.WriteStoreError(w, r, err)
		return
	}
	resp := uploadResponse{Package: pkgName, Version: verName, Name: fileName}
	if meta, err := f.Meta(r.Context()); err == nil {
		resp.Size = meta.Size
		resp.Digest = meta.Digest
	}
	surface.WriteJSON(w, http.StatusCreated, resp)
}

// upsertContainer creates a package or version (idempotent): add creates the
// .meta envelope, returning 201; if it already exists, the annotations (when
// supplied) are applied and 200 is returned. annotate replaces annotations
// wholesale, preserving CreatedAt.
func upsertContainer(ctx context.Context, existing annotatable, add func(opts ...core.CreateOption) error, ann map[string]any) (int, error) {
	var opts []core.CreateOption
	if len(ann) > 0 {
		opts = append(opts, core.WithAnnotations(ann))
	}
	err := add(opts...)
	if err == nil {
		return http.StatusCreated, nil
	}
	if !errors.Is(err, core.ErrAlreadyExists) {
		return 0, err
	}
	if len(ann) > 0 {
		if err := existing.Annotate(ctx, ann); err != nil {
			return 0, err
		}
	}
	return http.StatusOK, nil
}

// annotatable is the subset of Package/Version used to update annotations on an
// already-existing container.
type annotatable interface {
	Annotate(ctx context.Context, annotations map[string]any) error
}

// readCreate parses the optional {"annotations": {...}} body of a package or
// version PUT. An empty body means no annotations.
func (h *handler) readCreate(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	body, tooLarge, err := surface.ReadCappedBody(w, r, createBodyLimit)
	if tooLarge {
		surface.WriteError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return nil, false
	}
	if err != nil {
		surface.WriteError(w, http.StatusBadRequest, "invalid request body")
		return nil, false
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, true
	}
	var req createRequest
	if err := json.Unmarshal(body, &req); err != nil {
		surface.WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return nil, false
	}
	return req.Annotations, true
}
