package core

import (
	"context"
	"io"
)

// Store is the root handle for a single namespace. It is namespace-bound at
// construction; the namespace (a path prefix like "pypi/global") is never an
// argument to a Store method, only an output of Namespace.
//
// Deletion and yank verbs are out of scope for v1.
type Store interface {
	// Namespace returns the namespace this Store is bound to (for
	// example "pypi/global"). The same value is returned by Namespace
	// on every noun reachable through this Store.
	Namespace() string

	// Package returns a handle to the named Package without performing
	// any I/O. The returned Package may or may not exist in storage;
	// existence is observed only when a context-taking method is called.
	Package(name string) Package

	// Packages lists every Package present in the Store's namespace.
	Packages(ctx context.Context) ([]Package, error)

	// AddPackage creates a Package in storage. Options can carry
	// creation-time annotations via WithAnnotations.
	AddPackage(ctx context.Context, name string, opts ...CreateOption) (Package, error)

	// File returns a handle to a namespace-level File without performing I/O.
	File(name string) File

	// Files lists every namespace-level File in this Store.
	Files(ctx context.Context) ([]File, error)

	// AddFile uploads a namespace-level File. It behaves like Version.AddFile:
	// immutable by default, digesting during upload, and recording per-file Meta.
	AddFile(ctx context.Context, name string, body io.Reader, opts ...CreateOption) (File, error)

	// Cache returns a handle to a format-level cache file by key (the .cache/
	// subtree directly under the Store's namespace) without performing I/O.
	Cache(key string) CacheFile

	// AddCache writes a format-level cache file, mirroring AddFile but always
	// overwriting (the cache is mutable). Used by proxy surfaces to cache
	// upstream index/metadata; invisible to Packages.
	AddCache(ctx context.Context, key string, body io.Reader) (CacheFile, error)
}
