package core

import (
	"context"
	"io"
)

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

	// TagTargets resolves every dist-tag on this Package to its target version
	// in one call, returning a tag-name → version map. It exists so callers do
	// not hand-roll a list-then-resolve-each loop; each tag is still a separate
	// stored object, so the cost is one listing plus one read per tag.
	TagTargets(ctx context.Context) (map[string]string, error)

	// SetTag creates or updates a Tag, pointing name at the given target
	// version. Tag mutation is the only writable operation rooted on
	// Package (rather than Tag) because tags share a single backing
	// object per Package.
	SetTag(ctx context.Context, name, target string) error

	// Cache returns a handle to a package-level cache file by key (the .cache/
	// subtree under this Package) without performing I/O.
	Cache(key string) CacheFile

	// AddCache writes a package-level cache file, mirroring AddFile but always
	// overwriting (the cache is mutable). Used by proxy surfaces to cache a
	// package's upstream index/metadata; invisible to Versions/Files.
	AddCache(ctx context.Context, key string, body io.Reader) (CacheFile, error)
}
