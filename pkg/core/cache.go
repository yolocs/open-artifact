package core

import (
	"context"
	"time"
)

// CacheEntry is an opaque cached blob and the time it was written. Callers use
// ModTime to apply their own freshness/TTL policy.
type CacheEntry struct {
	Body    []byte
	ModTime time.Time
}

// Cache is a keyed store of opaque blobs scoped to one level of the hierarchy
// (Store, Package, or Version). It backs proxy pull-through caching of upstream
// index/metadata: a surface stores derived documents (a PyPI simple page, an
// npm packument) under a logical key and decides freshness itself from
// CacheEntry.ModTime.
//
// Cache contents live under a reserved .cache/ subtree at the owning level and
// are never returned as listed entries — a cache blob is not a Package, Version,
// or File. (Writing a package- or version-level cache does, like any object,
// materialize that package/version directory, so the package/version itself
// becomes observable in the parent listing. The format-level cache —
// Store.Cache, the proxy pull-through level — has no such effect.) Artifact
// files are NOT cached here — they are written as real Files via AddFile and
// served like any hosted artifact.
//
// Filling the cache is part of the read path: implementations authorize Cache
// operations as reads, so reader policy is sufficient to populate it.
type Cache interface {
	// Get returns the cached blob for key. A missing entry yields
	// (zero, false, nil).
	Get(ctx context.Context, key string) (CacheEntry, bool, error)

	// Put writes body as the cached blob for key, overwriting any existing
	// entry.
	Put(ctx context.Context, key string, body []byte) error

	// Delete removes the cached blob for key. A missing entry is not an error.
	Delete(ctx context.Context, key string) error
}
