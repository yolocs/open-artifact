package core

import "context"

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
