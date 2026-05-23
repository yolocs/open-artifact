package core

import (
	"context"
	"io"
)

// CacheFile is a single opaque cache blob. It is the same thing as a File —
// same storage, same read/digest/metadata machinery — differing only in where
// it lives (a reserved .cache/ folder at its level) and that it has no
// Package/Version parent. Proxy surfaces store derived upstream index/metadata
// (a PyPI simple page, an npm packument) as cache files and read freshness from
// Meta (UpdatedAt is the write time).
type CacheFile interface {
	// Name is the logical cache key as supplied to Cache.File/Put.
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
}

// Cache is a per-level store of opaque cache files, rooted at a reserved
// .cache/ folder under a Store, Package, or Version. Its files reuse the File
// implementation; they are invisible to the listing verbs
// (Packages/Versions/Files) because .cache/ is a dot-entry dropped at every
// level. Artifact files are NOT cached here — they are written as real Files
// via AddFile and served like any hosted artifact.
//
// Filling the cache is part of the read path: implementations authorize Cache
// operations as reads, so reader policy is sufficient to populate it.
type Cache interface {
	// File returns a handle to a cache file by key without performing I/O.
	File(name string) CacheFile

	// Put streams body to the cache file for key, overwriting any existing entry
	// (the cache is mutable), and returns its handle.
	Put(ctx context.Context, name string, body io.Reader, opts ...CreateOption) (CacheFile, error)

	// Delete removes the cache file for key. A missing entry is not an error.
	Delete(ctx context.Context, name string) error
}
