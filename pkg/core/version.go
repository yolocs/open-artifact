package core

import (
	"context"
	"io"
)

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
