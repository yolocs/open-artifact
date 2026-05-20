package blobstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
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
}

// Option customizes a Store at construction.
type Option func(*Store)

// withClock overrides the Store's time source (used by tests).
func withClock(now func() time.Time) Option {
	return func(s *Store) { s.now = now }
}

// NewWithBucket constructs a Store over b, bound to scope (a path prefix such
// as "pypi/global"). The bucket is the storage driver and is owned by the
// caller; the Store never closes it.
func NewWithBucket(b *blob.Bucket, scope string, opts ...Option) (*Store, error) {
	if b == nil {
		return nil, errors.New("blobstore: nil bucket")
	}
	s := &Store{
		bucket: b,
		scope:  scope,
		now:    time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// Namespace returns the scope this Store is bound to.
func (s *Store) Namespace() string { return s.scope }

// Package returns a handle to the named Package without performing any I/O.
func (s *Store) Package(name string) core.Package {
	return &pkg{store: s, name: name}
}

// Packages is filled in by Milestone 3 (listing).
func (s *Store) Packages(ctx context.Context) ([]core.Package, error) {
	return nil, core.ErrUnsupported
}

// AddPackage is filled in by Milestone 3.
func (s *Store) AddPackage(ctx context.Context, name string, opts ...core.CreateOption) (core.Package, error) {
	return nil, core.ErrUnsupported
}

// pkg is the blobstore implementation of core.Package.
type pkg struct {
	store *Store
	name  string
}

func (p *pkg) Name() string      { return p.name }
func (p *pkg) Namespace() string { return p.store.scope }
func (p *pkg) Store() core.Store { return p.store }

func (p *pkg) Meta(ctx context.Context) (core.Meta, error) { return core.Meta{}, core.ErrUnsupported }
func (p *pkg) Exists(ctx context.Context) (bool, error)    { return false, core.ErrUnsupported }
func (p *pkg) Annotate(ctx context.Context, annotations map[string]any) error {
	return core.ErrUnsupported
}

func (p *pkg) Version(name string) core.Version { return &version{pkg: p, name: name} }

func (p *pkg) Versions(ctx context.Context) ([]core.Version, error) {
	return nil, core.ErrUnsupported
}

func (p *pkg) AddVersion(ctx context.Context, name string, opts ...core.CreateOption) (core.Version, error) {
	return nil, core.ErrUnsupported
}

func (p *pkg) Tag(name string) core.Tag { return &tag{pkg: p, name: name} }

func (p *pkg) Tags(ctx context.Context) ([]core.Tag, error) { return nil, core.ErrUnsupported }

func (p *pkg) SetTag(ctx context.Context, name, target string) error { return core.ErrUnsupported }

// version is the blobstore implementation of core.Version.
type version struct {
	pkg  *pkg
	name string
}

func (v *version) Name() string          { return v.name }
func (v *version) Namespace() string     { return v.pkg.store.scope }
func (v *version) Package() core.Package { return v.pkg }

func (v *version) Meta(ctx context.Context) (core.Meta, error) {
	return core.Meta{}, core.ErrUnsupported
}
func (v *version) Exists(ctx context.Context) (bool, error) { return false, core.ErrUnsupported }
func (v *version) Annotate(ctx context.Context, annotations map[string]any) error {
	return core.ErrUnsupported
}

func (v *version) File(name string) core.File { return &file{version: v, name: name} }

func (v *version) Files(ctx context.Context) ([]core.File, error) { return nil, core.ErrUnsupported }

// AddFile streams body to the version's blob path while computing a rolling
// SHA256, then writes the per-file .meta.<file> sidecar carrying the digest
// and timestamps. With AllowOverwrite=false (the default) a pre-existing blob
// causes ErrAlreadyExists.
func (v *version) AddFile(ctx context.Context, name string, body io.Reader, opts ...core.CreateOption) (core.File, error) {
	cfg := core.NewCreateConfig(opts...)
	s := v.pkg.store
	blobPath := filePath(s.scope, v.pkg.name, v.name, name)

	if !cfg.AllowOverwrite {
		exists, err := s.bucket.Exists(ctx, blobPath)
		if err != nil {
			return nil, fmt.Errorf("blobstore: probe %q: %w", blobPath, mapErr(err))
		}
		if exists {
			return nil, core.ErrAlreadyExists
		}
	}

	w, err := s.bucket.NewWriter(ctx, blobPath, nil)
	if err != nil {
		return nil, fmt.Errorf("blobstore: open writer %q: %w", blobPath, mapErr(err))
	}

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(w, h), body); err != nil {
		// Abort the in-flight write so no partial blob is committed.
		_ = w.Close()
		return nil, fmt.Errorf("blobstore: stream %q: %w", blobPath, mapErr(err))
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("blobstore: commit %q: %w", blobPath, mapErr(err))
	}

	now := s.now().UTC()
	meta := core.Meta{
		Digest:      digestOf(h),
		CreatedAt:   now,
		UpdatedAt:   now,
		Annotations: cfg.Annotations,
	}
	if err := s.writeMeta(ctx, fileMetaPath(s.scope, v.pkg.name, v.name, name), meta); err != nil {
		// The blob is committed and reachable; the digest is recomputed
		// lazily on read if the sidecar is absent. Surface the error so the
		// caller knows the sidecar did not land.
		return nil, fmt.Errorf("blobstore: write sidecar for %q: %w", blobPath, err)
	}

	return &file{version: v, name: name}, nil
}

// file is the blobstore implementation of core.File.
type file struct {
	version *version
	name    string
}

func (f *file) Name() string          { return f.name }
func (f *file) Namespace() string     { return f.version.pkg.store.scope }
func (f *file) Version() core.Version { return f.version }
func (f *file) Package() core.Package { return f.version.pkg }

func (f *file) store() *Store { return f.version.pkg.store }
func (f *file) blobPath() string {
	return filePath(f.store().scope, f.version.pkg.name, f.version.name, f.name)
}
func (f *file) metaPath() string {
	return fileMetaPath(f.store().scope, f.version.pkg.name, f.version.name, f.name)
}

func (f *file) Exists(ctx context.Context) (bool, error) {
	exists, err := f.store().bucket.Exists(ctx, f.blobPath())
	if err != nil {
		return false, fmt.Errorf("blobstore: stat %q: %w", f.blobPath(), mapErr(err))
	}
	return exists, nil
}

// Meta returns the file's metadata envelope. It prefers the sidecar; when the
// sidecar is absent or corrupted it recomputes the digest by streaming the
// blob and derives timestamps from the bucket attributes.
func (f *file) Meta(ctx context.Context) (core.Meta, error) {
	s := f.store()
	raw, err := s.bucket.ReadAll(ctx, f.metaPath())
	if err == nil {
		if m, derr := decodeMeta(raw); derr == nil {
			return m, nil
		}
		// Corrupted sidecar: fall through to lazy recomputation.
	} else if gcerrors.Code(err) != gcerrors.NotFound {
		return core.Meta{}, fmt.Errorf("blobstore: read sidecar %q: %w", f.metaPath(), mapErr(err))
	}

	return f.recomputeMeta(ctx)
}

// recomputeMeta derives a Meta from the blob itself: digest from a streaming
// hash, UpdatedAt from the bucket's ModTime. Returns ErrNotFound if the blob
// is absent.
func (f *file) recomputeMeta(ctx context.Context) (core.Meta, error) {
	s := f.store()
	attrs, err := s.bucket.Attributes(ctx, f.blobPath())
	if err != nil {
		return core.Meta{}, fmt.Errorf("blobstore: attributes %q: %w", f.blobPath(), mapErr(err))
	}

	r, err := s.bucket.NewReader(ctx, f.blobPath(), nil)
	if err != nil {
		return core.Meta{}, fmt.Errorf("blobstore: open %q: %w", f.blobPath(), mapErr(err))
	}
	defer r.Close()

	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return core.Meta{}, fmt.Errorf("blobstore: hash %q: %w", f.blobPath(), mapErr(err))
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
	s := f.store()
	r, err := s.bucket.NewReader(ctx, f.blobPath(), nil)
	if err != nil {
		return nil, fmt.Errorf("blobstore: open %q: %w", f.blobPath(), mapErr(err))
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
	raw, err := f.store().bucket.ReadAll(ctx, f.metaPath())
	if err != nil {
		return ""
	}
	m, err := decodeMeta(raw)
	if err != nil {
		return ""
	}
	return m.Digest
}

// DownloadURL is stubbed for Milestone 2; the SignedURL cache lands in a later
// milestone.
func (f *file) DownloadURL(ctx context.Context) (string, error) {
	return "", core.ErrUnsupported
}

// tag is the blobstore implementation of core.Tag (stubbed for Milestone 3).
type tag struct {
	pkg  *pkg
	name string
}

func (t *tag) Name() string          { return t.name }
func (t *tag) Namespace() string     { return t.pkg.store.scope }
func (t *tag) Package() core.Package { return t.pkg }

func (t *tag) Ref(ctx context.Context) (core.Version, error) { return nil, core.ErrUnsupported }
func (t *tag) Exists(ctx context.Context) (bool, error)      { return false, core.ErrUnsupported }

// writeMeta encodes and writes a Meta envelope to path.
func (s *Store) writeMeta(ctx context.Context, path string, m core.Meta) error {
	b, err := encodeMeta(m)
	if err != nil {
		return fmt.Errorf("encode meta: %w", err)
	}
	if err := s.bucket.WriteAll(ctx, path, b, nil); err != nil {
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
