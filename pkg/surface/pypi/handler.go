package pypi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
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

const (
	pypiFormat = string(core.FormatPyPI)
)

const DefaultMaxUploadBytes int64 = 100 << 20

type Config struct {
	MaxUploadBytes      int64
	SimpleIndexCacheTTL time.Duration
}

func (c Config) uploadLimit() int64 {
	if c.MaxUploadBytes <= 0 {
		return DefaultMaxUploadBytes
	}
	return c.MaxUploadBytes
}

func Handler(reg *namespace.Registry, authn auth.Authenticator, cfg Config) http.Handler {
	h := &handler{
		reg:   reg,
		opts:  cfg,
		now:   time.Now,
		cache: newProjectCache(cfg.SimpleIndexCacheTTL, time.Now),
	}
	return auth.Middleware(authn)(h.router())
}

type handler struct {
	reg   *namespace.Registry
	opts  Config
	now   func() time.Time
	cache *projectCache
}

func (h *handler) router() http.Handler {
	r := mux.NewRouter()
	r.HandleFunc("/{namespace}", h.uploadRoute).Methods(http.MethodPost, http.MethodPut)
	r.HandleFunc("/{namespace}/", h.uploadRoute).Methods(http.MethodPost, http.MethodPut)
	r.HandleFunc("/{namespace}/simple", h.rootIndexRoute).Methods(http.MethodGet, http.MethodHead)
	r.HandleFunc("/{namespace}/simple/", h.rootIndexRoute).Methods(http.MethodGet, http.MethodHead)
	r.HandleFunc("/{namespace}/simple/{project}", h.projectIndexRoute).Methods(http.MethodGet, http.MethodHead)
	r.HandleFunc("/{namespace}/simple/{project}/", h.projectIndexRoute).Methods(http.MethodGet, http.MethodHead)
	r.HandleFunc("/{namespace}/packages/{project}/{version}/{filename}", h.downloadRoute).Methods(http.MethodGet, http.MethodHead)
	r.MethodNotAllowedHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		surface.WriteMethodNotAllowed(w, []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut})
	})
	return r
}

func (h *handler) uploadRoute(w http.ResponseWriter, r *http.Request) {
	observability.SetOperation(r, "upload")
	ns, ok := h.namespace(w, r, surface.NamespaceDataWrite)
	if !ok {
		return
	}
	h.upload(w, r, ns)
}

func (h *handler) rootIndexRoute(w http.ResponseWriter, r *http.Request) {
	observability.SetOperation(r, "simple.root")
	ns, ok := h.namespace(w, r, surface.NamespaceDataRead)
	if !ok {
		return
	}
	h.rootIndex(w, r, ns)
}

func (h *handler) projectIndexRoute(w http.ResponseWriter, r *http.Request) {
	observability.SetOperation(r, "simple.project")
	ns, ok := h.namespace(w, r, surface.NamespaceDataRead)
	if !ok {
		return
	}
	h.projectIndex(w, r, ns, mux.Vars(r)["project"])
}

func (h *handler) downloadRoute(w http.ResponseWriter, r *http.Request) {
	observability.SetOperation(r, "download")
	ns, ok := h.namespace(w, r, surface.NamespaceDataRead)
	if !ok {
		return
	}
	vars := mux.Vars(r)
	h.download(w, r, ns, vars["project"], vars["version"], vars["filename"])
}

func (h *handler) namespace(w http.ResponseWriter, r *http.Request, ctx surface.NamespaceErrorContext) (string, bool) {
	ns := mux.Vars(r)["namespace"]
	if err := namespace.ValidateName(ns); err != nil {
		surface.WriteNamespaceError(w, r, err, ctx)
		return "", false
	}
	observability.SetNamespace(r, ns)
	return ns, true
}

func (h *handler) upload(w http.ResponseWriter, r *http.Request, ns string) {
	store, err := h.authorizedHostedStore(w, r, ns, true)
	if err != nil {
		return
	}

	r = surface.WithMaxBody(w, r, h.opts.uploadLimit())
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		if strings.Contains(err.Error(), "request body too large") {
			surface.WriteError(w, http.StatusRequestEntityTooLarge, "upload too large")
			return
		}
		surface.WriteError(w, http.StatusBadRequest, "invalid multipart upload")
		return
	}
	defer r.MultipartForm.RemoveAll()
	project, err := NormalizeProject(r.FormValue("name"))
	if err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	version := r.FormValue("version")
	if err := ValidateVersion(version); err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	file, header, err := r.FormFile("content")
	if err != nil {
		surface.WriteError(w, http.StatusBadRequest, "missing content file")
		return
	}
	defer file.Close()
	filename := header.Filename
	if err := ValidateFilename(filename); err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	if declared := strings.TrimSpace(r.FormValue("sha256_digest")); declared != "" {
		if err := verifyDeclaredSHA256(file, declared); err != nil {
			surface.WriteStoreError(w, r, err)
			return
		}
	}

	annotations := uploadAnnotations(r, project, version, filename, h.now().UTC())
	if _, err := store.AddPackage(r.Context(), project, core.WithAnnotations(map[string]any{
		"pypi:name": project,
	})); err != nil && !errors.Is(err, core.ErrAlreadyExists) {
		surface.WriteStoreError(w, r, err)
		return
	}
	pkg := store.Package(project)
	if _, err := pkg.AddVersion(r.Context(), version, core.WithAnnotations(map[string]any{
		"pypi:version": version,
	})); err != nil && !errors.Is(err, core.ErrAlreadyExists) {
		surface.WriteStoreError(w, r, err)
		return
	}
	if _, err := pkg.Version(version).AddFile(r.Context(), filename, file, core.WithAnnotations(annotations)); err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	h.cache.invalidate(ns, project)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusCreated)
	_, _ = io.WriteString(w, "uploaded\n")
}

func uploadAnnotations(r *http.Request, project, version, filename string, uploadedAt time.Time) map[string]any {
	out := map[string]any{
		"pypi:name":        project,
		"pypi:version":     version,
		"pypi:filename":    filename,
		"pypi:uploaded_at": uploadedAt.Format(time.RFC3339Nano),
	}
	for form, key := range map[string]string{
		"filetype":         "pypi:filetype",
		"pyversion":        "pypi:pyversion",
		"metadata_version": "pypi:metadata_version",
		"summary":          "pypi:summary",
		"requires_python":  "pypi:requires_python",
		"sha256_digest":    "pypi:sha256_digest",
	} {
		if v := r.FormValue(form); v != "" {
			out[key] = v
		}
	}
	return out
}

func verifyDeclaredSHA256(file multipart.File, declared string) error {
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, strings.TrimSpace(declared)) {
		return core.ErrDigestMismatch
	}
	return nil
}

func (h *handler) rootIndex(w http.ResponseWriter, r *http.Request, ns string) {
	store, err := h.authorizedHostedStore(w, r, ns, false)
	if err != nil {
		return
	}
	pkgs, err := store.Packages(r.Context())
	if err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	projects := make([]Project, 0, len(pkgs))
	for _, p := range pkgs {
		projects = append(projects, Project{Name: p.Name()})
	}
	h.writeHTML(w, r, RenderRootHTML(projects))
}

func (h *handler) projectIndex(w http.ResponseWriter, r *http.Request, ns, rawProject string) {
	project, err := NormalizeProject(rawProject)
	if err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	store, err := h.authorizedHostedStore(w, r, ns, false)
	if err != nil {
		return
	}
	pkg := store.Package(project)
	exists, err := pkg.Exists(r.Context())
	if err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	if !exists {
		surface.WriteStoreError(w, r, core.ErrNotFound)
		return
	}

	page, err := h.cache.get(ns, project, func() (ProjectPage, error) {
		return buildProjectPage(r, ns, project, pkg)
	})
	if err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	if PrefersSimpleJSON(r.Header.Get("Accept")) {
		w.Header().Set("Content-Type", simpleJSONMediaType)
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(ProjectJSON(page))
		return
	}
	h.writeHTML(w, r, RenderProjectHTML(page))
}

func buildProjectPage(r *http.Request, ns, project string, pkg core.Package) (ProjectPage, error) {
	versions, err := pkg.Versions(r.Context())
	if err != nil {
		return ProjectPage{}, err
	}
	var links []FileLink
	for _, version := range versions {
		files, err := version.Files(r.Context())
		if err != nil {
			return ProjectPage{}, err
		}
		for _, file := range files {
			meta, err := file.Meta(r.Context())
			if err != nil {
				return ProjectPage{}, err
			}
			links = append(links, FileLink{
				Filename:       file.Name(),
				URL:            packageURL(ns, project, version.Name(), file.Name()),
				SHA256:         HashFromDigest(meta.Digest),
				RequiresPython: annotationString(meta.Annotations, "pypi:requires_python"),
			})
		}
	}
	sort.Slice(links, func(i, j int) bool {
		if links[i].Filename == links[j].Filename {
			return links[i].URL < links[j].URL
		}
		return links[i].Filename < links[j].Filename
	})
	return ProjectPage{Name: project, Files: links}, nil
}

func (h *handler) download(w http.ResponseWriter, r *http.Request, ns, rawProject, version, filename string) {
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
	store, err := h.authorizedHostedStore(w, r, ns, false)
	if err != nil {
		return
	}
	surface.RedirectOrStreamFile(w, r, store.Package(project).Version(version).File(filename), "application/octet-stream")
}

func (h *handler) authorizedHostedStore(w http.ResponseWriter, r *http.Request, ns string, write bool) (core.Store, error) {
	ac := auth.FromContext(r.Context())
	store, spec, err := h.reg.AuthorizedStore(r.Context(), ns, pypiFormat, ac)
	if err != nil {
		ctx := surface.NamespaceDataRead
		if write {
			ctx = surface.NamespaceDataWrite
		}
		surface.WriteNamespaceError(w, r, err, ctx)
		return nil, err
	}
	if spec.IsProxy() {
		surface.WriteStoreError(w, r, core.ErrUnsupported)
		return nil, core.ErrUnsupported
	}
	return store, nil
}

func (h *handler) writeHTML(w http.ResponseWriter, r *http.Request, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.WriteString(w, body)
}

func packageURL(ns, project, version, filename string) string {
	return "/" + url.PathEscape(ns) +
		"/packages/" + url.PathEscape(project) +
		"/" + url.PathEscape(version) +
		"/" + url.PathEscape(filename)
}

func annotationString(annotations map[string]any, key string) string {
	if annotations == nil {
		return ""
	}
	v, _ := annotations[key].(string)
	return v
}

type projectCache struct {
	ttl        time.Duration
	now        func() time.Time
	mu         sync.Mutex
	generation uint64
	m          map[string]projectCacheEntry
}

type projectCacheEntry struct {
	page    ProjectPage
	expires time.Time
}

func newProjectCache(ttl time.Duration, now func() time.Time) *projectCache {
	return &projectCache{
		ttl: ttl,
		now: now,
		m:   make(map[string]projectCacheEntry),
	}
}

func (c *projectCache) get(ns, project string, load func() (ProjectPage, error)) (ProjectPage, error) {
	if c.ttl <= 0 {
		return load()
	}
	key := cacheKey(ns, project)
	c.mu.Lock()
	if e, ok := c.m[key]; ok && c.now().Before(e.expires) {
		c.mu.Unlock()
		return e.page, nil
	}
	generation := c.generation
	c.mu.Unlock()

	page, err := load()
	if err != nil {
		return ProjectPage{}, err
	}
	c.mu.Lock()
	if generation == c.generation {
		c.m[key] = projectCacheEntry{page: page, expires: c.now().Add(c.ttl)}
	}
	c.mu.Unlock()
	return page, nil
}

func (c *projectCache) invalidate(ns, project string) {
	if c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	c.generation++
	delete(c.m, cacheKey(ns, project))
	c.mu.Unlock()
}

func cacheKey(ns, project string) string {
	return ns + "\x00" + project
}
