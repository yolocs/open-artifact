package core

import (
	"context"
	"io"
)

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
