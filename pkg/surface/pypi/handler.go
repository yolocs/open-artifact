package pypi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yolocs/open-artifact/pkg/auth"
	"github.com/yolocs/open-artifact/pkg/core"
	"github.com/yolocs/open-artifact/pkg/namespace"
	"github.com/yolocs/open-artifact/pkg/surface"
)

const (
	defaultSimpleIndexCacheTTL = 60 * time.Second
	pypiFormat                 = string(core.FormatPyPI)
)

type Option func(*options)

type options struct {
	maxUploadBytes     int64
	simpleIndexCacheTT time.Duration
	now                func() time.Time
}

func WithMaxUploadBytes(n int64) Option {
	return func(o *options) {
		o.maxUploadBytes = n
	}
}

func WithSimpleIndexCacheTTL(ttl time.Duration) Option {
	return func(o *options) {
		o.simpleIndexCacheTT = ttl
	}
}

func Handler(reg *namespace.Registry, authn auth.Authenticator, opts ...Option) http.Handler {
	cfg := options{simpleIndexCacheTT: defaultSimpleIndexCacheTTL, now: time.Now}
	for _, opt := range opts {
		opt(&cfg)
	}
	h := &handler{
		reg:   reg,
		opts:  cfg,
		cache: newProjectCache(cfg.simpleIndexCacheTT, cfg.now),
	}
	return auth.Middleware(authn)(http.HandlerFunc(h.route))
}

type handler struct {
	reg   *namespace.Registry
	opts  options
	cache *projectCache
}

func (h *handler) route(w http.ResponseWriter, r *http.Request) {
	ns, rest, ok := splitNamespace(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := namespace.ValidateName(ns); err != nil {
		surface.WriteNamespaceError(w, r, err, surface.NamespaceDataRead)
		return
	}

	if rest == "" || rest == "/" {
		switch r.Method {
		case http.MethodPost, http.MethodPut:
			h.upload(w, r, ns)
		default:
			surface.WriteMethodNotAllowed(w, []string{http.MethodPost, http.MethodPut})
		}
		return
	}

	switch {
	case rest == "/simple" || rest == "/simple/":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			surface.WriteMethodNotAllowed(w, []string{http.MethodGet, http.MethodHead})
			return
		}
		h.rootIndex(w, r, ns)
	case strings.HasPrefix(rest, "/simple/"):
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			surface.WriteMethodNotAllowed(w, []string{http.MethodGet, http.MethodHead})
			return
		}
		project := strings.TrimSuffix(strings.TrimPrefix(rest, "/simple/"), "/")
		if project == "" || strings.Contains(project, "/") {
			http.NotFound(w, r)
			return
		}
		h.projectIndex(w, r, ns, project)
	case strings.HasPrefix(rest, "/packages/"):
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			surface.WriteMethodNotAllowed(w, []string{http.MethodGet, http.MethodHead})
			return
		}
		parts := strings.Split(strings.TrimPrefix(rest, "/packages/"), "/")
		if len(parts) != 3 {
			http.NotFound(w, r)
			return
		}
		h.download(w, r, ns, parts[0], parts[1], parts[2])
	default:
		http.NotFound(w, r)
	}
}

func splitNamespace(path string) (string, string, bool) {
	trimmed := strings.TrimPrefix(path, "/")
	if trimmed == "" {
		return "", "", false
	}
	head, tail, found := strings.Cut(trimmed, "/")
	if !found {
		return head, "", true
	}
	return head, "/" + tail, true
}

func (h *handler) upload(w http.ResponseWriter, r *http.Request, ns string) {
	store, err := h.authorizedStore(w, r, ns, true)
	if err != nil {
		return
	}
	if err := h.ensureHosted(r, ns); err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}

	r = surface.WithMaxBody(w, r, h.opts.maxUploadBytes)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		if strings.Contains(err.Error(), "request body too large") {
			surface.WriteError(w, http.StatusRequestEntityTooLarge, "upload too large")
			return
		}
		surface.WriteError(w, http.StatusBadRequest, "invalid multipart upload")
		return
	}
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

	annotations := uploadAnnotations(r, project, version, filename, h.opts.now().UTC())
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
	} {
		if v := r.FormValue(form); v != "" {
			out[key] = v
		}
	}
	return out
}

func (h *handler) rootIndex(w http.ResponseWriter, r *http.Request, ns string) {
	store, err := h.authorizedStore(w, r, ns, false)
	if err != nil {
		return
	}
	pkgs, err := store.Packages(r.Context())
	if err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	if err := h.ensureHosted(r, ns); err != nil {
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
	store, err := h.authorizedStore(w, r, ns, false)
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
	if err := h.ensureHosted(r, ns); err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}

	page, err := h.cache.get(ns, project, func() (ProjectPage, error) {
		return buildProjectPage(r, ns, project, pkg)
	})
	if err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	if WantsJSON(r.Header.Get("Accept")) {
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
	store, err := h.authorizedStore(w, r, ns, false)
	if err != nil {
		return
	}
	if err := h.ensureHosted(r, ns); err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	surface.RedirectOrStreamFile(w, r, store.Package(project).Version(version).File(filename), "application/octet-stream")
}

func (h *handler) authorizedStore(w http.ResponseWriter, r *http.Request, ns string, write bool) (core.Store, error) {
	ac := auth.FromContext(r.Context())
	store, err := h.reg.Authorized(r.Context(), ns, pypiFormat, ac)
	if err != nil {
		ctx := surface.NamespaceDataRead
		if write {
			ctx = surface.NamespaceAdminWrite
		}
		surface.WriteNamespaceError(w, r, err, ctx)
		return nil, err
	}
	return store, nil
}

func (h *handler) ensureHosted(r *http.Request, ns string) error {
	scoped, err := h.reg.For(ns, pypiFormat)
	if err != nil {
		return err
	}
	spec, err := scoped.Spec(r.Context())
	if err != nil {
		return err
	}
	if spec.IsProxy() {
		return fmt.Errorf("%w: PyPI proxy mode is not implemented", core.ErrUnsupported)
	}
	return nil
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
	ttl time.Duration
	now func() time.Time
	mu  sync.Mutex
	m   map[string]projectCacheEntry
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
	c.mu.Unlock()

	page, err := load()
	if err != nil {
		return ProjectPage{}, err
	}
	c.mu.Lock()
	c.m[key] = projectCacheEntry{page: page, expires: c.now().Add(c.ttl)}
	c.mu.Unlock()
	return page, nil
}

func (c *projectCache) invalidate(ns, project string) {
	if c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	delete(c.m, cacheKey(ns, project))
	c.mu.Unlock()
}

func cacheKey(ns, project string) string {
	return ns + "\x00" + project
}
