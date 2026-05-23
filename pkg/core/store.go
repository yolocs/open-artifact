package core

import "context"

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

	// Cache returns the format-level opaque blob cache (the .cache/ subtree
	// directly under the Store's namespace). Used by proxy surfaces to cache
	// upstream index/metadata; invisible to Packages.
	Cache() Cache
}
