package core

import (
	"context"
	"io"
)

// The nouns of open-artifact are chainable handles, not value structs. A
// handle is obtained without I/O; existence and contents are observed only
// when a context-taking method is called. Handles compose downward from a
// Store:
//
//	file := store.Package("requests").Version("2.31.0").
//		File("requests-2.31.0-py3-none-any.whl")
//	rc, err := file.Read(ctx)

// Package is a handle to a named artifact within a Store's namespace.
type Package interface {
	// Name is the package name local to its Store (for example
	// "requests" or "@databricks/sdk").
	Name() string

	// Namespace returns the namespace of the owning Store.
	Namespace() string

	// Store returns the parent Store.
	Store() Store

	// Meta returns the Package's metadata envelope.
	Meta(ctx context.Context) (Meta, error)

	// Exists reports whether the Package is present in storage.
	// Implementations should prefer a cheap probe of the Package's
	// .meta object and fall back to a descendant check (any Version,
	// Tag, or File written under the Package) only when the probe
	// returns ErrNotFound.
	Exists(ctx context.Context) (bool, error)

	// Annotate updates the Package's annotations map. Other Meta fields
	// (CreatedAt, UpdatedAt) are managed by the implementation.
	Annotate(ctx context.Context, annotations map[string]any) error

	// Version returns a handle to the named Version without performing
	// any I/O.
	Version(name string) Version

	// Versions lists every Version under this Package.
	Versions(ctx context.Context) ([]Version, error)

	// AddVersion creates a Version under this Package. Options can carry
	// creation-time annotations via WithAnnotations.
	AddVersion(ctx context.Context, name string, opts ...CreateOption) (Version, error)

	// Tag returns a handle to the named Tag without performing any I/O.
	Tag(name string) Tag

	// Tags lists every Tag under this Package.
	Tags(ctx context.Context) ([]Tag, error)

	// SetTag creates or updates a Tag, pointing name at the given target
	// version. Tag mutation is the only writable operation rooted on
	// Package (rather than Tag) because tags share a single backing
	// object per Package.
	SetTag(ctx context.Context, name, target string) error
}

// Version is a handle to a single release of a Package.
type Version interface {
	// Name is the canonical version string (for example "2.31.0").
	Name() string

	// Namespace returns the namespace of the owning Store.
	Namespace() string

	// Package returns the parent Package.
	Package() Package

	// Meta returns the Version's metadata envelope.
	Meta(ctx context.Context) (Meta, error)

	// Exists reports whether the Version is present in storage.
	// Implementations should prefer a cheap probe of the Version's
	// .meta object and fall back to a descendant check (any File
	// written under the Version) only when the probe returns
	// ErrNotFound.
	Exists(ctx context.Context) (bool, error)

	// Annotate updates the Version's annotations map.
	Annotate(ctx context.Context, annotations map[string]any) error

	// File returns a handle to the named File without performing any
	// I/O.
	File(name string) File

	// Files lists every File under this Version.
	Files(ctx context.Context) ([]File, error)

	// AddFile uploads a File under this Version. body is streamed to
	// storage; the implementation computes the digest during upload and
	// records it on the resulting File's Meta.
	AddFile(ctx context.Context, name string, body io.Reader, opts ...CreateOption) (File, error)
}

// Tag is a handle to a named alias (a dist-tag) within a Package.
type Tag interface {
	// Name is the tag name (for example "latest" or "beta").
	Name() string

	// Namespace returns the namespace of the owning Store.
	Namespace() string

	// Package returns the parent Package.
	Package() Package

	// Ref resolves the Tag to its current Version.
	Ref(ctx context.Context) (Version, error)

	// Exists reports whether the Tag is present in the owning
	// Package's tag map. It is equivalent to a successful Ref but
	// avoids resolving the target Version.
	Exists(ctx context.Context) (bool, error)
}

// File is a handle to a single blob within a Version.
type File interface {
	// Name is the user-visible file name (for example
	// "requests-2.31.0-py3-none-any.whl").
	Name() string

	// Namespace returns the namespace of the owning Store.
	Namespace() string

	// Version returns the parent Version.
	Version() Version

	// Package returns the grandparent Package. It is a convenience over
	// f.Version().Package() and performs no I/O.
	Package() Package

	// Meta returns the File's metadata envelope (which carries the
	// recorded digest).
	Meta(ctx context.Context) (Meta, error)

	// Exists reports whether the File blob is present in storage.
	// Implementations should answer with a single Stat on the blob path.
	Exists(ctx context.Context) (bool, error)

	// Read returns an open reader over the file's bytes. The caller is
	// responsible for closing the reader.
	Read(ctx context.Context) (io.ReadCloser, error)

	// DownloadURL returns a pre-signed URL the caller can hand to a
	// client for direct download from the backing store. Implementations
	// without redirect support return an empty string and a nil error;
	// callers should fall back to Read in that case.
	DownloadURL(ctx context.Context) (string, error)
}
