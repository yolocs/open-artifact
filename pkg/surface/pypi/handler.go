package pypi

import (
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"sort"
	"strings"

	"github.com/yolocs/open-artifact/pkg/core"
	"github.com/yolocs/open-artifact/pkg/surface"
)

const (
	// maxTextFieldBytes caps any single non-file multipart field (name,
	// version, :action, sha256_digest, ...). Every legitimate twine field is
	// well under 1 KiB; 8 KiB is a generous margin.
	maxTextFieldBytes = 8 << 10
	// maxTotalTextFieldBytes caps the cumulative size of all text fields in an
	// upload, defending against many sub-cap fields piling up.
	maxTotalTextFieldBytes = 64 << 10
	// DefaultMaxUploadBytes caps the total multipart upload body. The walker
	// never spills to disk, so this only bounds a single streaming request;
	// 1 GiB comfortably exceeds any wheel or sdist on PyPI.
	DefaultMaxUploadBytes = 1 << 30
)

var errFieldTooLarge = errors.New("pypi: form field exceeds limit")

// Handler is the inbound PyPI surface. It serves PEP 503/691 simple indexes,
// twine uploads, and file downloads, all backed by a core.Store. When an
// upstream client is configured the handler proxies and caches through it:
// index entries from the upstream are merged into the served index, and a
// download miss is filled from the upstream and persisted into the Store.
type Handler struct {
	store          core.Store
	upstream       *UpstreamClient
	prefix         string
	maxUploadBytes int64
}

// Option customizes a Handler.
type Option func(*Handler)

// WithMaxUploadBytes caps the total size of a multipart upload body. The
// default is DefaultMaxUploadBytes (1 GiB). A non-positive value disables the
// cap (not recommended).
func WithMaxUploadBytes(n int64) Option {
	return func(h *Handler) { h.maxUploadBytes = n }
}

// NewHandler constructs a PyPI surface over store. upstream may be nil for a
// hosted-only deployment; when non-nil the handler proxies and caches through
// it.
func NewHandler(store core.Store, upstream *UpstreamClient, opts ...Option) *Handler {
	h := &Handler{
		store:          store,
		upstream:       upstream,
		maxUploadBytes: DefaultMaxUploadBytes,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

var _ surface.Handler = (*Handler)(nil)

// Mount registers the surface's routes on mux under prefix. The patterns use
// the net/http method-and-wildcard matcher (Go 1.22+); a GET pattern also
// answers HEAD.
func (h *Handler) Mount(prefix string, mux *http.ServeMux) {
	base := strings.TrimRight(prefix, "/")
	h.prefix = base

	mux.HandleFunc("GET "+base+"/simple/{$}", h.handleTopLevel)
	mux.HandleFunc("GET "+base+"/simple/{package}/{$}", h.handleProjectIndex)
	mux.HandleFunc("GET "+base+"/simple/{package}", h.redirectToSlash)
	mux.HandleFunc("GET "+base+"/packages/{package}/{version}/{filename}", h.handleFileGet)
	mux.HandleFunc("POST "+base+"/{$}", h.handleUpload)
}

// fileURL builds the absolute download path for a file served by this surface.
// Segments are validated to a path-safe character set, so no escaping is
// needed.
func (h *Handler) fileURL(pkg, version, filename string) string {
	return h.prefix + "/packages/" + pkg + "/" + version + "/" + filename
}

// redirectToSlash sends /simple/{package} to /simple/{package}/ — pip always
// uses the trailing slash, but a stray request shouldn't 404.
func (h *Handler) redirectToSlash(w http.ResponseWriter, req *http.Request) {
	http.Redirect(w, req, req.URL.Path+"/", http.StatusMovedPermanently)
}

// handleTopLevel serves the root simple index: every project known locally,
// merged with the upstream's project list when a proxy is configured.
func (h *Handler) handleTopLevel(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	seen := map[string]bool{}
	var names []string
	add := func(n string) {
		if n == "" || seen[n] {
			return
		}
		seen[n] = true
		names = append(names, n)
	}

	pkgs, err := h.store.Packages(ctx)
	if surface.WriteStoreError(w, err) {
		return
	}
	for _, p := range pkgs {
		add(p.Name())
	}

	if h.upstream != nil {
		if upNames, uerr := h.upstream.TopLevel(ctx); uerr == nil {
			for _, n := range upNames {
				add(normalize(n))
			}
		} else if !errors.Is(uerr, ErrUpstreamNotFound) && len(names) == 0 {
			h.writeUpstreamError(w, uerr)
			return
		}
	}

	sort.Strings(names)
	writeProjectList(w, req.Header.Get("Accept"), h.prefix, names)
}

// handleProjectIndex serves /simple/{package}/: the union of locally stored
// files and (when proxying) the upstream's advertised files, with download
// URLs rewritten to route through this surface.
func (h *Handler) handleProjectIndex(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	pkg, err := validatePackageName(req.PathValue("package"))
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	seen := map[string]bool{}
	var files []indexFile

	versions, err := h.store.Package(pkg).Versions(ctx)
	if surface.WriteStoreError(w, err) {
		return
	}
	for _, v := range versions {
		vfiles, ferr := v.Files(ctx)
		if surface.WriteStoreError(w, ferr) {
			return
		}
		for _, f := range vfiles {
			sha := ""
			if m, merr := f.Meta(ctx); merr == nil {
				sha = strings.TrimPrefix(m.Digest, "sha256:")
			}
			files = append(files, indexFile{
				Filename: f.Name(),
				URL:      h.fileURL(pkg, v.Name(), f.Name()),
				Sha256:   sha,
			})
			seen[f.Name()] = true
		}
	}

	if h.upstream != nil {
		proj, uerr := h.upstream.Project(ctx, pkg)
		switch {
		case uerr == nil:
			for _, uf := range proj.Files {
				if seen[uf.Filename] {
					continue
				}
				ver, perr := parseFilenameVersion(uf.Filename, pkg)
				if perr != nil {
					continue
				}
				files = append(files, indexFile{
					Filename: uf.Filename,
					URL:      h.fileURL(pkg, ver, uf.Filename),
					Sha256:   uf.SHA256(),
				})
				seen[uf.Filename] = true
			}
		case errors.Is(uerr, ErrUpstreamNotFound):
			// Upstream doesn't know the project; local files (if any) stand.
		default:
			if len(files) == 0 {
				h.writeUpstreamError(w, uerr)
				return
			}
			// Degraded: serve the local view rather than fail outright.
		}
	}

	if len(files) == 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	sortIndexFiles(files)
	writeProjectIndex(w, req.Header.Get("Accept"), pkg, files)
}

// handleFileGet serves a file download. It serves from the Store when present
// (307 redirect to a signed URL when the backend supports it, else a stream),
// and otherwise — when proxying — fills from the upstream while teeing the
// bytes to the client. HEAD short-circuits to a presence check.
func (h *Handler) handleFileGet(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	isHead := req.Method == http.MethodHead

	pkg, err := validatePackageName(req.PathValue("package"))
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	version := req.PathValue("version")
	if err := validateVersion(version); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	filename := req.PathValue("filename")
	if err := validateFilename(filename); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	file := h.store.Package(pkg).Version(version).File(filename)
	served, err := h.serveFromStore(ctx, w, req, file, isHead)
	if surface.WriteStoreError(w, err) {
		return
	}
	if served {
		return
	}

	// Local miss.
	if h.upstream == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	h.fillFromUpstream(ctx, w, req, pkg, version, filename, isHead)
}

// serveFromStore serves file from the Store. It returns (true, nil) when the
// response was written, (false, nil) on a clean miss, and a non-nil error for
// a backend failure the caller should map.
func (h *Handler) serveFromStore(ctx context.Context, w http.ResponseWriter, req *http.Request, file core.File, isHead bool) (bool, error) {
	exists, err := file.Exists(ctx)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}

	if isHead {
		w.Header().Set("Content-Type", detectMediaType(file.Name()))
		w.WriteHeader(http.StatusOK)
		return true, nil
	}

	if url, derr := file.DownloadURL(ctx); derr == nil && url != "" {
		http.Redirect(w, req, url, http.StatusTemporaryRedirect)
		return true, nil
	}

	w.Header().Set("Content-Type", detectMediaType(file.Name()))
	rc, err := file.Read(ctx)
	if err != nil {
		return false, err
	}
	defer rc.Close()
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
	return true, nil
}

// fillFromUpstream resolves the file's upstream URL via the project index,
// fetches it, persists it into the Store, and serves it to the client. On GET
// the bytes are teed to the client as they stream into the Store; on HEAD the
// body is drained into the Store and a 200 is returned.
func (h *Handler) fillFromUpstream(ctx context.Context, w http.ResponseWriter, req *http.Request, pkg, version, filename string, isHead bool) {
	proj, err := h.upstream.Project(ctx, pkg)
	if err != nil {
		if errors.Is(err, ErrUpstreamNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		h.writeUpstreamError(w, err)
		return
	}
	uf, ok := proj.Find(filename)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	fr, err := h.upstream.FetchFile(ctx, uf.URL)
	if err != nil {
		if errors.Is(err, ErrUpstreamNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		h.writeUpstreamError(w, err)
		return
	}
	defer fr.Body.Close()

	if err := h.ensurePackageVersion(ctx, pkg, version); err != nil {
		surface.WriteStoreError(w, err)
		return
	}
	ver := h.store.Package(pkg).Version(version)

	if isHead {
		// Drain into the Store to populate the cache, then report presence.
		if _, aerr := ver.AddFile(ctx, filename, fr.Body); aerr != nil && !errors.Is(aerr, core.ErrAlreadyExists) {
			surface.WriteStoreError(w, aerr)
			return
		}
		w.Header().Set("Content-Type", detectMediaType(filename))
		w.WriteHeader(http.StatusOK)
		return
	}

	w.Header().Set("Content-Type", detectMediaType(filename))
	sniff := &sniffWriter{w: w}
	tee := io.TeeReader(fr.Body, sniff)
	if _, aerr := ver.AddFile(ctx, filename, tee); aerr != nil {
		if errors.Is(aerr, core.ErrAlreadyExists) {
			// A concurrent fill won the race; AddFile returns before reading
			// the body, so nothing was teed to the client yet. Serve the now-
			// cached copy.
			if served, serr := h.serveFromStore(ctx, w, req, ver.File(filename), false); served || surface.WriteStoreError(w, serr) {
				return
			}
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if sniff.wrote {
			// Response body already started; status/headers are committed and
			// appending an error would corrupt the bytes the client is
			// hashing. Bail and let the client's next request retry.
			return
		}
		surface.WriteStoreError(w, aerr)
		return
	}
}

// ensurePackageVersion creates the package and version envelopes if absent,
// treating ErrAlreadyExists as success so concurrent fills/uploads converge.
func (h *Handler) ensurePackageVersion(ctx context.Context, pkg, version string) error {
	if _, err := h.store.AddPackage(ctx, pkg); err != nil && !errors.Is(err, core.ErrAlreadyExists) {
		return err
	}
	if _, err := h.store.Package(pkg).AddVersion(ctx, version); err != nil && !errors.Is(err, core.ErrAlreadyExists) {
		return err
	}
	return nil
}

// handleUpload accepts a twine-style multipart upload and stores the file.
func (h *Handler) handleUpload(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	if h.maxUploadBytes > 0 {
		req.Body = http.MaxBytesReader(w, req.Body, h.maxUploadBytes)
	}

	mr, err := req.MultipartReader()
	if err != nil {
		switch {
		case errors.Is(err, http.ErrNotMultipart), errors.Is(err, http.ErrMissingBoundary):
			http.Error(w, "expected a multipart upload", http.StatusBadRequest)
		default:
			http.Error(w, "invalid form data", http.StatusBadRequest)
		}
		return
	}

	fields := map[string]string{}
	var totalText int
	var content *multipart.Part
	for {
		p, perr := mr.NextPart()
		if errors.Is(perr, io.EOF) {
			break
		}
		if perr != nil {
			http.Error(w, "invalid form data", http.StatusBadRequest)
			return
		}
		if p.FormName() == "content" {
			content = p
			break
		}
		v, rerr := readTextPart(p, maxTextFieldBytes)
		_ = p.Close()
		if rerr != nil {
			if errors.Is(rerr, errFieldTooLarge) {
				http.Error(w, "form field too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "invalid form data", http.StatusBadRequest)
			return
		}
		totalText += len(v)
		if totalText > maxTotalTextFieldBytes {
			http.Error(w, "form fields too large", http.StatusRequestEntityTooLarge)
			return
		}
		if v != "" {
			if _, ok := fields[p.FormName()]; !ok {
				fields[p.FormName()] = v
			}
		}
	}
	if content == nil {
		http.Error(w, "missing content part", http.StatusBadRequest)
		return
	}
	defer content.Close()

	pkg, err := validatePackageName(fields["name"])
	if err != nil {
		http.Error(w, "invalid package name", http.StatusBadRequest)
		return
	}
	version := fields["version"]
	if err := validateVersion(version); err != nil {
		http.Error(w, "invalid version", http.StatusBadRequest)
		return
	}
	filename := content.FileName()
	if err := validateFilename(filename); err != nil {
		http.Error(w, "invalid filename", http.StatusBadRequest)
		return
	}

	if err := h.ensurePackageVersion(ctx, pkg, version); err != nil {
		surface.WriteStoreError(w, err)
		return
	}
	ver := h.store.Package(pkg).Version(version)
	if _, err := ver.AddFile(ctx, filename, content); err != nil {
		if mbErr := (*http.MaxBytesError)(nil); errors.As(err, &mbErr) {
			http.Error(w, "upload too large", http.StatusRequestEntityTooLarge)
			return
		}
		surface.WriteStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// writeUpstreamError maps an upstream client error to its HTTP response.
func (h *Handler) writeUpstreamError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrUpstreamNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	default:
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}
}

// readTextPart reads a multipart text field, capping it at limit bytes.
func readTextPart(p *multipart.Part, limit int64) (string, error) {
	b, err := io.ReadAll(io.LimitReader(p, limit+1))
	if int64(len(b)) > limit {
		return "", errFieldTooLarge
	}
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// sniffWriter records whether any bytes were written so the cache-through path
// can tell when the response body has been committed.
type sniffWriter struct {
	w     io.Writer
	wrote bool
}

func (s *sniffWriter) Write(p []byte) (int, error) {
	if len(p) > 0 {
		s.wrote = true
	}
	return s.w.Write(p)
}

// detectMediaType returns the download Content-Type for a distribution
// filename. PyPI serves binaries as application/octet-stream; PEP 658 metadata
// sidecars are plain text.
func detectMediaType(filename string) string {
	if strings.HasSuffix(filename, ".metadata") {
		return "text/plain; charset=utf-8"
	}
	return "application/octet-stream"
}
