package core

import (
	"context"
	"io"
)

// CacheFile is a single opaque cache blob. It mirrors File — same storage, same
// read/digest/metadata machinery — differing only in where it lives (a reserved
// .cache/ folder at its level), in being mutable and evictable, and in having
// no Package/Version parent. Proxy surfaces store derived upstream
// index/metadata (a PyPI simple page, an npm packument) as cache files and read
// freshness from Meta (UpdatedAt is the write time).
//
// A handle is obtained without I/O via Store/Package/Version.Cache(key);
// existence and contents are observed only when a context-taking method is
// called.
type CacheFile interface {
	// Name is the logical cache key the handle was obtained with.
	Name() string

	// Exists reports whether the cache blob is present.
	Exists(ctx context.Context) (bool, error)

	// Read returns an open reader over the cached bytes (digest-verified, like a
	// File). The caller closes it.
	Read(ctx context.Context) (io.ReadCloser, error)

	// Meta returns the cache file's metadata envelope (digest, write
	// timestamps). UpdatedAt is the freshness anchor.
	Meta(ctx context.Context) (Meta, error)

	// DownloadURL returns a pre-signed URL for the cached blob, or "" when the
	// backend has no signing support (caller falls back to Read). Cache hits may
	// redirect because the URL targets the operator's own bucket.
	DownloadURL(ctx context.Context) (string, error)

	// Delete removes the cache blob. A missing entry is not an error. Unlike a
	// File, a cache entry is evictable.
	Delete(ctx context.Context) error
}
