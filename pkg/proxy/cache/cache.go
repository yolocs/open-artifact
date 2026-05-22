// Package cache is the proxy's blob-backed metadata cache: a thin keyed blob
// store under a format's .cache/ folder, for proxy-mode surfaces to cache
// upstream package/version index documents.
//
// It stores opaque bytes and nothing more. Artifact files fetched from upstream
// are NOT cached here — they are written into the namespace's core.Store as
// real Packages/Versions/Files and served from there like any hosted artifact,
// so a cold miss permanently populates the registry. Only derived
// index/metadata (a PyPI simple page, an npm packument) belongs in .cache/, and
// it is entirely up to the surface format to decide what to cache, under what
// key, and how long to trust it. Get reports each entry's ModTime so the
// surface can apply its own freshness policy.
//
// Entries live under the format-level cache directory (an optional
// bucket-prefix is inserted after the fixed root):
//
//	open-artifact/v1/<prefix>/<ns>/<fmt>/.cache/<sha256(key)>
//
// The logical key (e.g. "simple:requests" or "packument:@scope/name") is hashed
// into a single path-safe segment. Everything under .cache/ is invisible to
// core.Store listings because a leading-dot entry is dropped at every level.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"gocloud.dev/blob"
	"gocloud.dev/gcerrors"

	"github.com/yolocs/open-artifact/pkg/core/blobstore"
)

const cacheDir = ".cache/"

// Entry is a cached blob and the time it was written. The surface uses ModTime
// to apply its own freshness/TTL policy.
type Entry struct {
	Body    []byte
	ModTime time.Time
}

// Cache reads and writes opaque cache blobs on a blob bucket.
type Cache struct {
	bucket *blob.Bucket
	prefix string
}

// New constructs a Cache over b. bucketPrefix is the optional deployment prefix
// (the same one passed to the namespace registry), inserted after the fixed
// root. The bucket is owned by the caller; the Cache never closes it.
func New(b *blob.Bucket, bucketPrefix string) (*Cache, error) {
	if b == nil {
		return nil, errors.New("cache: nil bucket")
	}
	return &Cache{bucket: b, prefix: bucketPrefix}, nil
}

// Get returns the cached blob for key. A missing entry yields (zero, false, nil).
func (c *Cache) Get(ctx context.Context, ns, format, key string) (Entry, bool, error) {
	r, err := c.bucket.NewReader(ctx, c.path(ns, format, key), nil)
	if err != nil {
		if gcerrors.Code(err) == gcerrors.NotFound {
			return Entry{}, false, nil
		}
		return Entry{}, false, fmt.Errorf("cache: open %q: %w", key, err)
	}
	defer r.Close()

	body, err := io.ReadAll(r)
	if err != nil {
		return Entry{}, false, fmt.Errorf("cache: read %q: %w", key, err)
	}
	return Entry{Body: body, ModTime: r.ModTime()}, true, nil
}

// Put writes body as the cached blob for key, overwriting any existing entry.
func (c *Cache) Put(ctx context.Context, ns, format, key string, body []byte) error {
	if err := c.bucket.WriteAll(ctx, c.path(ns, format, key), body, nil); err != nil {
		return fmt.Errorf("cache: write %q: %w", key, err)
	}
	return nil
}

// Delete removes the cached blob for key. A missing entry is not an error.
func (c *Cache) Delete(ctx context.Context, ns, format, key string) error {
	if err := c.bucket.Delete(ctx, c.path(ns, format, key)); err != nil && gcerrors.Code(err) != gcerrors.NotFound {
		return fmt.Errorf("cache: delete %q: %w", key, err)
	}
	return nil
}

// path returns the on-bucket object path for a cache key.
func (c *Cache) path(ns, format, key string) string {
	p := blobstore.Root
	if c.prefix != "" {
		p += c.prefix + "/"
	}
	return p + ns + "/" + format + "/" + cacheDir + hashKey(key)
}

// hashKey renders the logical key as a hex sha256, the path-safe stand-in used
// in the object name.
func hashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}
