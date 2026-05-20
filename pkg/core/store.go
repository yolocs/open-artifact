package core

import (
	"context"
	"io"
)

// Store is the storage contract for a single scope. It is scope-blind at the
// type level: the scope (a path prefix like "pypi/global") is fixed when a
// concrete Store is constructed and never appears in a method signature.
//
// Deletion and yank verbs are out of scope for v1.
type Store interface {
	// ListPackages returns every package in the scope.
	ListPackages(ctx context.Context) ([]Package, error)
	// ListVersions returns every version of pkg.
	ListVersions(ctx context.Context, pkg Package) ([]Version, error)
	// ListTags returns every dist-tag of pkg.
	ListTags(ctx context.Context, pkg Package) ([]Tag, error)
	// ResolveTag returns the Version that the named tag of pkg points at.
	ResolveTag(ctx context.Context, pkg Package, name string) (Version, error)
	// SetTag creates or updates a dist-tag.
	SetTag(ctx context.Context, tag Tag) error
	// ListFiles returns every file in ver.
	ListFiles(ctx context.Context, ver Version) ([]File, error)
	// AddFile streams r into the file's blob, verifying the digest, and
	// returns the stored File with any fields the Store computed (digest,
	// size) filled in.
	AddFile(ctx context.Context, file File, r io.Reader) (File, error)
	// ReadFile opens the file's blob for reading. The caller must close the
	// returned ReadCloser.
	ReadFile(ctx context.Context, file File) (File, io.ReadCloser, error)
	// BlobRedirectURL returns a URL a client can fetch the blob from
	// directly (e.g. a signed object-store URL), or ErrUnsupported when the
	// backend cannot issue one.
	BlobRedirectURL(ctx context.Context, file File) (string, error)
}
