package blobstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"gocloud.dev/blob"
	"gocloud.dev/gcerrors"

	"github.com/yolocs/open-artifact/pkg/core"
)

// Store is a core.Store backed by a single gocloud.dev/blob bucket. It is
// bound to one scope at construction; the scope is never a method argument,
// only an output of Namespace.
type Store struct {
	bucket *blob.Bucket
	scope  string
	now    func() time.Time

	// sign and stat abstract the bucket's SignedURL and Attributes methods so
	// the facade-transparent caches can be tested in isolation.
	sign signFunc
	stat statFunc

	urlTTL  time.Duration
	statTTL time.Duration

	urlCache  *memoCache[string]
	statCache *memoCache[*blob.Attributes]

	guard   Guard
	metrics Metrics
}

// Guard authorizes an operation against the Store's scope before it reaches the
// bucket. write distinguishes a mutation from a read. It returns nil to allow
// and a non-nil error to deny; the Store surfaces that error unchanged. A nil
// Guard disables the check, so a Store constructed without one is a plain,
// trusted storage handle (used by the admin plane and tests).
//
// The hook is deliberately auth-agnostic: blobstore knows nothing about OIDC,
// namespaces, or policy. The namespace layer supplies a Guard that closes over
// a compiled policy and the request's subject, keeping pkg/core free of those
// concerns while making authorization impossible to bypass at the storage
// boundary.
type Guard func(ctx context.Context, write bool) error

// Option customizes a Store at construction.
type Option func(*Store)

// withClock overrides the Store's time source (used by tests).
func withClock(now func() time.Time) Option {
	return func(s *Store) { s.now = now }
}

// WithGuard installs an authorization Guard. Every read/write the Store
// performs is authorized through g before any bucket access.
func WithGuard(g Guard) Option {
	return func(s *Store) { s.guard = g }
}

// NewWithBucket constructs a Store over b, bound to scope (a path prefix such
// as "pypi/global"). The bucket is the storage driver and is owned by the
// caller; the Store never closes it.
func NewWithBucket(b *blob.Bucket, scope string, opts ...Option) (*Store, error) {
	if b == nil {
		return nil, errors.New("blobstore: nil bucket")
	}
	s := &Store{
		bucket:  b,
		scope:   scope,
		now:     time.Now,
		urlTTL:  defaultURLCacheTTL,
		statTTL: defaultStatCacheTTL,
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.sign == nil {
		s.sign = b.SignedURL
	}
	if s.stat == nil {
		s.stat = b.Attributes
	}
	// The signed URL is requested for s.urlTTL; cache the entry for slightly
	// less so it is always evicted before the URL it holds can expire (the
	// clamp the issue calls for). Signing latency only widens this margin.
	s.urlCache = newMemoCache[string](defaultURLCacheCap, clampCacheTTL(s.urlTTL), s.now)
	s.statCache = newMemoCache[*blob.Attributes](defaultStatCacheCap, s.statTTL, s.now)
	return s, nil
}

// Namespace returns the scope this Store is bound to.
func (s *Store) Namespace() string { return s.scope }

// authorize runs the Guard for an operation, if one is installed. write reports
// whether the operation mutates storage.
func (s *Store) authorize(ctx context.Context, write bool) error {
	if s.guard == nil {
		return nil
	}
	return s.guard(ctx, write)
}

// Package returns a handle to the named Package without performing any I/O. An
// invalid name (empty or leading-dot) is not rejected here — the handle carries
// the error and every I/O method on it returns ErrInvalidName.
func (s *Store) Package(name string) core.Package {
	return &pkg{store: s, name: name, nameErr: validateName(name)}
}

// Packages lists every Package present in the Store's namespace. An empty
// namespace yields an empty slice, not an error.
func (s *Store) Packages(ctx context.Context) ([]core.Package, error) {
	if err := s.authorize(ctx, false); err != nil {
		return nil, err
	}
	names, err := s.listChildNames(ctx, scopePrefix(s.scope))
	if err != nil {
		return nil, err
	}
	out := make([]core.Package, 0, len(names))
	for _, n := range names {
		out = append(out, &pkg{store: s, name: decodeSegment(n)})
	}
	return out, nil
}

// AddPackage creates a Package's .meta envelope. With AllowOverwrite=false
// (the default) a pre-existing .meta yields ErrAlreadyExists.
func (s *Store) AddPackage(ctx context.Context, name string, opts ...core.CreateOption) (core.Package, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, true); err != nil {
		return nil, err
	}
	cfg := core.NewCreateConfig(opts...)
	path := packageMetaPath(s.scope, name)
	if !cfg.AllowOverwrite {
		exists, err := s.bExists(ctx, path)
		if err != nil {
			return nil, fmt.Errorf("blobstore: probe %q: %w", path, mapErr(err))
		}
		if exists {
			return nil, core.ErrAlreadyExists
		}
	}
	now := s.now().UTC()
	meta := core.Meta{CreatedAt: now, UpdatedAt: now, Annotations: cfg.Annotations}
	if err := s.writeMeta(ctx, path, meta); err != nil {
		return nil, err
	}
	return &pkg{store: s, name: name}, nil
}

// Cache returns a handle to a format-level cache file (under the .cache/
// directory directly beneath the Store's scope) without performing I/O.
func (s *Store) Cache(key string) core.CacheFile {
	return newCacheFile(s, scopePrefix(s.scope), key, nil)
}

// AddCache writes a format-level cache file, overwriting any existing entry.
func (s *Store) AddCache(ctx context.Context, key string, body io.Reader) (core.CacheFile, error) {
	return s.addCache(ctx, scopePrefix(s.scope), key, nil, body)
}

// addCache is the shared write path behind every level's AddCache. parentErr
// carries the owning Package/Version's name validation (nil at the format
// level). The cache is always mutable, so the write unconditionally overwrites.
// Filling the cache is part of serving a read (a proxy cold fill), so it
// authorizes as a read — reader policy is sufficient.
func (s *Store) addCache(ctx context.Context, dir, key string, parentErr error, body io.Reader) (core.CacheFile, error) {
	f := newCacheFile(s, dir, key, parentErr)
	if f.nameErr != nil {
		return nil, f.nameErr
	}
	if err := s.authorize(ctx, false); err != nil {
		return nil, err
	}
	if _, err := s.writeFile(ctx, f.blobKey, f.metaKey, body, core.CreateConfig{AllowOverwrite: true}); err != nil {
		return nil, err
	}
	return f, nil
}

// listChildNames lists the immediate, non-dot children under prefix using a
// "/" delimiter, returning their base names sorted. Dot-entries (the
// Store-owned .meta/.tags/.cache objects) are dropped at every level — one
// rule, every level.
func (s *Store) listChildNames(ctx context.Context, prefix string) ([]string, error) {
	start := time.Now()
	iter := s.bucket.List(&blob.ListOptions{Prefix: prefix, Delimiter: "/"})
	var names []string
	for {
		obj, err := iter.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			s.observe(opList, start, err)
			return nil, fmt.Errorf("blobstore: list %q: %w", prefix, mapErr(err))
		}
		name := strings.TrimSuffix(strings.TrimPrefix(obj.Key, prefix), "/")
		if name == "" || isDotEntry(name) {
			continue
		}
		names = append(names, name)
	}
	s.observe(opList, start, nil)
	sort.Strings(names)
	return names, nil
}

// hasDescendant reports whether any object exists under prefix. It backs the
// descendant fallback in Package/Version Exists: a record with no .meta still
// exists if anything was written beneath it.
func (s *Store) hasDescendant(ctx context.Context, prefix string) (bool, error) {
	start := time.Now()
	iter := s.bucket.List(&blob.ListOptions{Prefix: prefix})
	_, err := iter.Next(ctx)
	if errors.Is(err, io.EOF) {
		s.observe(opList, start, nil)
		return false, nil
	}
	if err != nil {
		s.observe(opList, start, err)
		return false, fmt.Errorf("blobstore: list %q: %w", prefix, mapErr(err))
	}
	s.observe(opList, start, nil)
	return true, nil
}

// readMeta reads and decodes a .meta envelope at path, mapping a missing
// object to ErrNotFound.
func (s *Store) readMeta(ctx context.Context, path string) (core.Meta, error) {
	raw, err := s.bReadAll(ctx, path)
	if err != nil {
		return core.Meta{}, fmt.Errorf("blobstore: read meta %q: %w", path, mapErr(err))
	}
	m, err := decodeMeta(raw)
	if err != nil {
		return core.Meta{}, fmt.Errorf("blobstore: decode meta %q: %w", path, err)
	}
	return m, nil
}

// upsertAnnotations applies annotations to the .meta at path, preserving
// CreatedAt when the envelope already exists and bumping UpdatedAt.
func (s *Store) upsertAnnotations(ctx context.Context, path string, annotations map[string]any) error {
	m, err := s.readMeta(ctx, path)
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		return err
	}
	now := s.now().UTC()
	if m.CreatedAt.IsZero() {
		m.CreatedAt = now
	}
	m.UpdatedAt = now
	m.Annotations = annotations
	return s.writeMeta(ctx, path, m)
}

// pkg is the blobstore implementation of core.Package. nameErr is set when the
// handle was built from an invalid name; every I/O method returns it.
type pkg struct {
	store   *Store
	name    string
	nameErr error
}

func (p *pkg) Name() string      { return p.name }
func (p *pkg) Namespace() string { return p.store.scope }
func (p *pkg) Store() core.Store { return p.store }

func (p *pkg) Meta(ctx context.Context) (core.Meta, error) {
	if p.nameErr != nil {
		return core.Meta{}, p.nameErr
	}
	if err := p.store.authorize(ctx, false); err != nil {
		return core.Meta{}, err
	}
	return p.store.readMeta(ctx, packageMetaPath(p.store.scope, p.name))
}

func (p *pkg) Exists(ctx context.Context) (bool, error) {
	if p.nameErr != nil {
		return false, p.nameErr
	}
	s := p.store
	if err := s.authorize(ctx, false); err != nil {
		return false, err
	}
	ok, err := s.bExists(ctx, packageMetaPath(s.scope, p.name))
	if err != nil {
		return false, fmt.Errorf("blobstore: probe %q: %w", packageMetaPath(s.scope, p.name), mapErr(err))
	}
	if ok {
		return true, nil
	}
	return s.hasDescendant(ctx, packagePrefix(s.scope, p.name))
}

func (p *pkg) Annotate(ctx context.Context, annotations map[string]any) error {
	if p.nameErr != nil {
		return p.nameErr
	}
	if err := p.store.authorize(ctx, true); err != nil {
		return err
	}
	return p.store.upsertAnnotations(ctx, packageMetaPath(p.store.scope, p.name), annotations)
}

func (p *pkg) Version(name string) core.Version {
	return &version{pkg: p, name: name, nameErr: firstErr(p.nameErr, validateName(name))}
}

func (p *pkg) Versions(ctx context.Context) ([]core.Version, error) {
	if p.nameErr != nil {
		return nil, p.nameErr
	}
	s := p.store
	if err := s.authorize(ctx, false); err != nil {
		return nil, err
	}
	names, err := s.listChildNames(ctx, packagePrefix(s.scope, p.name))
	if err != nil {
		return nil, err
	}
	out := make([]core.Version, 0, len(names))
	for _, n := range names {
		out = append(out, &version{pkg: p, name: decodeSegment(n)})
	}
	return out, nil
}

func (p *pkg) AddVersion(ctx context.Context, name string, opts ...core.CreateOption) (core.Version, error) {
	if err := firstErr(p.nameErr, validateName(name)); err != nil {
		return nil, err
	}
	s := p.store
	if err := s.authorize(ctx, true); err != nil {
		return nil, err
	}
	cfg := core.NewCreateConfig(opts...)
	path := versionMetaPath(s.scope, p.name, name)
	if !cfg.AllowOverwrite {
		exists, err := s.bExists(ctx, path)
		if err != nil {
			return nil, fmt.Errorf("blobstore: probe %q: %w", path, mapErr(err))
		}
		if exists {
			return nil, core.ErrAlreadyExists
		}
	}
	now := s.now().UTC()
	meta := core.Meta{CreatedAt: now, UpdatedAt: now, Annotations: cfg.Annotations}
	if err := s.writeMeta(ctx, path, meta); err != nil {
		return nil, err
	}
	return &version{pkg: p, name: name}, nil
}

// Cache returns a handle to a package-level cache file without performing I/O.
func (p *pkg) Cache(key string) core.CacheFile {
	return newCacheFile(p.store, packagePrefix(p.store.scope, p.name), key, p.nameErr)
}

// AddCache writes a package-level cache file, overwriting any existing entry.
func (p *pkg) AddCache(ctx context.Context, key string, body io.Reader) (core.CacheFile, error) {
	return p.store.addCache(ctx, packagePrefix(p.store.scope, p.name), key, p.nameErr, body)
}

func (p *pkg) Tag(name string) core.Tag {
	return &tag{pkg: p, name: name, nameErr: firstErr(p.nameErr, validateName(name))}
}

func (p *pkg) Tags(ctx context.Context) ([]core.Tag, error) {
	if p.nameErr != nil {
		return nil, p.nameErr
	}
	s := p.store
	if err := s.authorize(ctx, false); err != nil {
		return nil, err
	}
	names, err := s.listChildNames(ctx, packageTagsPrefix(s.scope, p.name))
	if err != nil {
		return nil, err
	}
	out := make([]core.Tag, 0, len(names))
	for _, n := range names {
		out = append(out, &tag{pkg: p, name: decodeSegment(n)})
	}
	return out, nil
}

func (p *pkg) SetTag(ctx context.Context, name, target string) error {
	if err := firstErr(p.nameErr, validateName(name), validateName(target)); err != nil {
		return err
	}
	if err := p.store.authorize(ctx, true); err != nil {
		return err
	}
	return p.store.writeTagTarget(ctx, p.name, name, target)
}

// version is the blobstore implementation of core.Version. nameErr carries this
// handle's (and its package's) name validation.
type version struct {
	pkg     *pkg
	name    string
	nameErr error
}

func (v *version) Name() string          { return v.name }
func (v *version) Namespace() string     { return v.pkg.store.scope }
func (v *version) Package() core.Package { return v.pkg }

func (v *version) Meta(ctx context.Context) (core.Meta, error) {
	if v.nameErr != nil {
		return core.Meta{}, v.nameErr
	}
	s := v.pkg.store
	if err := s.authorize(ctx, false); err != nil {
		return core.Meta{}, err
	}
	return s.readMeta(ctx, versionMetaPath(s.scope, v.pkg.name, v.name))
}

func (v *version) Exists(ctx context.Context) (bool, error) {
	if v.nameErr != nil {
		return false, v.nameErr
	}
	s := v.pkg.store
	if err := s.authorize(ctx, false); err != nil {
		return false, err
	}
	path := versionMetaPath(s.scope, v.pkg.name, v.name)
	ok, err := s.bExists(ctx, path)
	if err != nil {
		return false, fmt.Errorf("blobstore: probe %q: %w", path, mapErr(err))
	}
	if ok {
		return true, nil
	}
	return s.hasDescendant(ctx, versionPrefix(s.scope, v.pkg.name, v.name))
}

func (v *version) Annotate(ctx context.Context, annotations map[string]any) error {
	if v.nameErr != nil {
		return v.nameErr
	}
	s := v.pkg.store
	if err := s.authorize(ctx, true); err != nil {
		return err
	}
	return s.upsertAnnotations(ctx, versionMetaPath(s.scope, v.pkg.name, v.name), annotations)
}

// Cache returns a handle to a version-level cache file without performing I/O.
func (v *version) Cache(key string) core.CacheFile {
	return newCacheFile(v.pkg.store, versionPrefix(v.pkg.store.scope, v.pkg.name, v.name), key, v.nameErr)
}

// AddCache writes a version-level cache file, overwriting any existing entry.
func (v *version) AddCache(ctx context.Context, key string, body io.Reader) (core.CacheFile, error) {
	return v.pkg.store.addCache(ctx, versionPrefix(v.pkg.store.scope, v.pkg.name, v.name), key, v.nameErr, body)
}

func (v *version) File(name string) core.File {
	return newFile(v, name)
}

func (v *version) Files(ctx context.Context) ([]core.File, error) {
	if v.nameErr != nil {
		return nil, v.nameErr
	}
	s := v.pkg.store
	if err := s.authorize(ctx, false); err != nil {
		return nil, err
	}
	names, err := s.listChildNames(ctx, versionPrefix(s.scope, v.pkg.name, v.name))
	if err != nil {
		return nil, err
	}
	out := make([]core.File, 0, len(names))
	for _, n := range names {
		out = append(out, newFile(v, decodeSegment(n)))
	}
	return out, nil
}

// AddFile streams body to the version's blob path while computing a rolling
// SHA256, then writes the per-file .meta.<file> sidecar carrying the digest
// and timestamps. With AllowOverwrite=false (the default) a pre-existing blob
// causes ErrAlreadyExists.
func (v *version) AddFile(ctx context.Context, name string, body io.Reader, opts ...core.CreateOption) (core.File, error) {
	f := newFile(v, name)
	if f.nameErr != nil {
		return nil, f.nameErr
	}
	s := v.pkg.store
	if err := s.authorize(ctx, true); err != nil {
		return nil, err
	}
	if _, err := s.writeFile(ctx, f.blobKey, f.metaKey, body, core.NewCreateConfig(opts...)); err != nil {
		return nil, err
	}
	return f, nil
}

// writeFile streams body to blobKey while computing a rolling SHA256, then
// writes the .meta sidecar at metaKey carrying the digest and timestamps. With
// AllowOverwrite=false a pre-existing blob yields ErrAlreadyExists. It is the
// shared write path behind AddFile and Cache.Put; it does not authorize —
// callers do, mapping the operation to read or write as appropriate.
func (s *Store) writeFile(ctx context.Context, blobKey, metaKey string, body io.Reader, cfg core.CreateConfig) (core.Meta, error) {
	if !cfg.AllowOverwrite {
		exists, err := s.bExists(ctx, blobKey)
		if err != nil {
			return core.Meta{}, fmt.Errorf("blobstore: probe %q: %w", blobKey, mapErr(err))
		}
		if exists {
			return core.Meta{}, core.ErrAlreadyExists
		}
	}

	// The blob writer is created under a child context we can cancel. On a
	// mid-stream error, calling Close alone would commit the bytes written so
	// far as a complete object (true for memblob/fileblob); cancelling the
	// writer's context before Close aborts the upload so no partial, servable
	// blob is left behind. This matters for streaming callers (a proxy cache
	// fill, a disconnected upload) where the body can end early.
	wctx, cancelWrite := context.WithCancel(ctx)
	defer cancelWrite()
	w, err := s.bNewWriter(wctx, blobKey, nil)
	if err != nil {
		return core.Meta{}, fmt.Errorf("blobstore: open writer %q: %w", blobKey, mapErr(err))
	}

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(w, h), body); err != nil {
		// Abort the in-flight write so no partial blob is committed.
		cancelWrite()
		_ = s.closeWriter(w)
		return core.Meta{}, fmt.Errorf("blobstore: stream %q: %w", blobKey, mapErr(err))
	}
	if err := s.closeWriter(w); err != nil {
		return core.Meta{}, fmt.Errorf("blobstore: commit %q: %w", blobKey, mapErr(err))
	}

	now := s.now().UTC()
	meta := core.Meta{
		Digest:      digestOf(h),
		CreatedAt:   now,
		UpdatedAt:   now,
		Annotations: cfg.Annotations,
	}
	if err := s.writeMeta(ctx, metaKey, meta); err != nil {
		// The blob is committed and reachable; the digest is recomputed lazily
		// on read if the sidecar is absent. Surface the error so the caller
		// knows the sidecar did not land.
		return core.Meta{}, fmt.Errorf("blobstore: write sidecar %q: %w", metaKey, err)
	}
	return meta, nil
}

// file is the blobstore implementation of core.File. It is path-based: blobKey
// and metaKey are computed once at construction, so the same type backs both a
// Version's files and a Cache's files (which have no Version parent). version is
// nil for cache files. nameErr carries the handle's (and its ancestors') name
// validation.
type file struct {
	store   *Store
	name    string
	blobKey string
	metaKey string
	version *version
	nameErr error
}

// newFile builds a handle to a Version's file.
func newFile(v *version, name string) *file {
	s := v.pkg.store
	return &file{
		store:   s,
		name:    name,
		blobKey: filePath(s.scope, v.pkg.name, v.name, name),
		metaKey: fileMetaPath(s.scope, v.pkg.name, v.name, name),
		version: v,
		nameErr: firstErr(v.nameErr, validateName(name)),
	}
}

// newCacheFile builds a handle to a cache file under the .cache/ folder of the
// level whose prefix is dir. parentErr carries the owning level's name
// validation (nil at the format level).
func newCacheFile(s *Store, dir, key string, parentErr error) *file {
	return &file{
		store:   s,
		name:    key,
		blobKey: cacheFilePath(dir, key),
		metaKey: cacheMetaPath(dir, key),
		nameErr: firstErr(parentErr, validateName(key)),
	}
}

func (f *file) Name() string      { return f.name }
func (f *file) Namespace() string { return f.store.scope }
func (f *file) Version() core.Version {
	if f.version == nil {
		return nil
	}
	return f.version
}
func (f *file) Package() core.Package {
	if f.version == nil {
		return nil
	}
	return f.version.pkg
}

func (f *file) Exists(ctx context.Context) (bool, error) {
	if f.nameErr != nil {
		return false, f.nameErr
	}
	if err := f.store.authorize(ctx, false); err != nil {
		return false, err
	}
	exists, err := f.store.bExists(ctx, f.blobKey)
	if err != nil {
		return false, fmt.Errorf("blobstore: stat %q: %w", f.blobKey, mapErr(err))
	}
	return exists, nil
}

// Meta returns the file's metadata envelope. It prefers the sidecar; when the
// sidecar is absent or corrupted it recomputes the digest by streaming the
// blob and derives timestamps from the bucket attributes.
func (f *file) Meta(ctx context.Context) (core.Meta, error) {
	if f.nameErr != nil {
		return core.Meta{}, f.nameErr
	}
	s := f.store
	if err := s.authorize(ctx, false); err != nil {
		return core.Meta{}, err
	}
	raw, err := s.bReadAll(ctx, f.metaKey)
	if err == nil {
		if m, derr := decodeMeta(raw); derr == nil {
			return m, nil
		}
		// Corrupted sidecar: fall through to lazy recomputation.
	} else if gcerrors.Code(err) != gcerrors.NotFound {
		return core.Meta{}, fmt.Errorf("blobstore: read sidecar %q: %w", f.metaKey, mapErr(err))
	}

	return f.recomputeMeta(ctx)
}

// recomputeMeta derives a Meta from the blob itself: digest from a streaming
// hash, UpdatedAt from the bucket's ModTime. Returns ErrNotFound if the blob
// is absent.
func (f *file) recomputeMeta(ctx context.Context) (core.Meta, error) {
	s := f.store
	attrs, err := s.attributes(ctx, f.blobKey)
	if err != nil {
		return core.Meta{}, err
	}

	r, err := s.bNewReader(ctx, f.blobKey, nil)
	if err != nil {
		return core.Meta{}, fmt.Errorf("blobstore: open %q: %w", f.blobKey, mapErr(err))
	}
	defer r.Close()

	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return core.Meta{}, fmt.Errorf("blobstore: hash %q: %w", f.blobKey, mapErr(err))
	}

	return core.Meta{
		Digest:    digestOf(h),
		UpdatedAt: attrs.ModTime.UTC(),
	}, nil
}

// Read returns a reader over the file's bytes. When a sidecar digest is
// present, the returned reader verifies the streamed content against it and
// surfaces ErrDigestMismatch at EOF.
func (f *file) Read(ctx context.Context) (io.ReadCloser, error) {
	if f.nameErr != nil {
		return nil, f.nameErr
	}
	s := f.store
	if err := s.authorize(ctx, false); err != nil {
		return nil, err
	}
	r, err := s.bNewReader(ctx, f.blobKey, nil)
	if err != nil {
		return nil, fmt.Errorf("blobstore: open %q: %w", f.blobKey, mapErr(err))
	}

	want := f.sidecarDigest(ctx)
	if want == "" {
		return r, nil
	}
	return &verifyingReader{r: r, h: sha256.New(), want: want}, nil
}

// sidecarDigest returns the digest recorded in the file's sidecar, or "" when
// the sidecar is absent or unreadable (digest verification is then skipped).
func (f *file) sidecarDigest(ctx context.Context) string {
	raw, err := f.store.bReadAll(ctx, f.metaKey)
	if err != nil {
		return ""
	}
	m, err := decodeMeta(raw)
	if err != nil {
		return ""
	}
	return m.Digest
}

// DownloadURL returns a pre-signed download URL through the facade-transparent
// URL cache (LRU + singleflight). Backends without signing support (memblob,
// fileblob) report Unimplemented; that miss is cached as an empty string so
// the surface falls back to streaming Read without re-probing.
func (f *file) DownloadURL(ctx context.Context) (string, error) {
	if f.nameErr != nil {
		return "", f.nameErr
	}
	s := f.store
	if err := s.authorize(ctx, false); err != nil {
		return "", err
	}
	key := f.blobKey
	u, err := s.urlCache.getOrCompute(key, func() (string, error) {
		// The SignedURL expiry equals the cache TTL, clamped by the backend's
		// own per-cloud maximum, so a cached URL never outlives its validity.
		start := time.Now()
		u, err := s.sign(ctx, key, &blob.SignedURLOptions{
			Expiry: s.urlTTL,
			Method: http.MethodGet,
		})
		s.observe(opSignedURL, start, err)
		if err != nil {
			if gcerrors.Code(err) == gcerrors.Unimplemented {
				return "", nil
			}
			return "", fmt.Errorf("blobstore: sign %q: %w", key, mapErr(err))
		}
		return u, nil
	})
	// Record what the surface will do with this result: redirect to a signed
	// URL, stream inline because signing is unsupported, or fall back to
	// streaming because signing failed.
	switch {
	case err != nil:
		s.redirect("error")
	case u == "":
		s.redirect("inline")
	default:
		s.redirect("redirected")
	}
	return u, err
}

// Delete removes the file's blob and .meta sidecar. It is exposed only through
// core.CacheFile (cache entries are evictable); regular Files have no Delete in
// v1. A missing object is not an error. It authorizes as a read, like the rest
// of the cache lifecycle.
func (f *file) Delete(ctx context.Context) error {
	if f.nameErr != nil {
		return f.nameErr
	}
	if err := f.store.authorize(ctx, false); err != nil {
		return err
	}
	for _, key := range []string{f.blobKey, f.metaKey} {
		if err := f.store.bDelete(ctx, key); err != nil && gcerrors.Code(err) != gcerrors.NotFound {
			return fmt.Errorf("blobstore: delete %q: %w", key, mapErr(err))
		}
	}
	return nil
}

// tag is the blobstore implementation of core.Tag. nameErr carries this
// handle's (and its package's) name validation.
type tag struct {
	pkg     *pkg
	name    string
	nameErr error
}

func (t *tag) Name() string          { return t.name }
func (t *tag) Namespace() string     { return t.pkg.store.scope }
func (t *tag) Package() core.Package { return t.pkg }

func (t *tag) Ref(ctx context.Context) (core.Version, error) {
	if t.nameErr != nil {
		return nil, t.nameErr
	}
	if err := t.pkg.store.authorize(ctx, false); err != nil {
		return nil, err
	}
	target, err := t.pkg.store.readTagTarget(ctx, t.pkg.name, t.name)
	if err != nil {
		return nil, err
	}
	return &version{pkg: t.pkg, name: target}, nil
}

func (t *tag) Exists(ctx context.Context) (bool, error) {
	if t.nameErr != nil {
		return false, t.nameErr
	}
	s := t.pkg.store
	if err := s.authorize(ctx, false); err != nil {
		return false, err
	}
	path := tagPath(s.scope, t.pkg.name, t.name)
	ok, err := s.bExists(ctx, path)
	if err != nil {
		return false, fmt.Errorf("blobstore: probe %q: %w", path, mapErr(err))
	}
	return ok, nil
}

// writeMeta encodes and writes a Meta envelope to path.
func (s *Store) writeMeta(ctx context.Context, path string, m core.Meta) error {
	b, err := encodeMeta(m)
	if err != nil {
		return fmt.Errorf("encode meta: %w", err)
	}
	if err := s.bWriteAll(ctx, path, b, nil); err != nil {
		return fmt.Errorf("write %q: %w", path, mapErr(err))
	}
	return nil
}

// verifyingReader streams the blob while hashing it, returning
// ErrDigestMismatch in place of io.EOF when the computed digest disagrees with
// the expected one.
type verifyingReader struct {
	r    io.ReadCloser
	h    hash.Hash
	want string
	done bool
}

func (v *verifyingReader) Read(p []byte) (int, error) {
	n, err := v.r.Read(p)
	if n > 0 {
		v.h.Write(p[:n])
	}
	if errors.Is(err, io.EOF) && !v.done {
		v.done = true
		if got := digestOf(v.h); got != v.want {
			return n, core.ErrDigestMismatch
		}
	}
	return n, err
}

func (v *verifyingReader) Close() error { return v.r.Close() }

// digestOf renders a finished hash as "sha256:<hex>".
func digestOf(h hash.Hash) string {
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// mapErr translates gocloud NotFound errors to core.ErrNotFound, passing other
// errors through unchanged.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if gcerrors.Code(err) == gcerrors.NotFound {
		return core.ErrNotFound
	}
	return err
}
