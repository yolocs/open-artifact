// Package cache is the proxy's blob-backed pull-through metadata+body cache. It
// stores upstream responses in the same bucket as everything else, under each
// format's .cache/ folder, which is invisible to core.Store listings because a
// leading-dot entry is dropped at every level.
//
// Layout (mirroring the canonical namespace-rooted scheme; an optional
// bucket-prefix is inserted after the root):
//
//	open-artifact/v1/<prefix>/<ns>/<fmt>/.cache/<sha256(key)>.body  ← cached body
//	open-artifact/v1/<prefix>/<ns>/<fmt>/.cache/<sha256(key)>.json  ← cached metadata
//
// The key is a stable logical string such as "pypi:simple:requests" or
// "npm:packument:@scope/name"; the path uses sha256(key) to dodge length and
// slash problems. The original key is kept in the metadata envelope for
// debugging and collision detection.
//
// The cache is mutable: callers overwrite entries as upstream changes. It holds
// only positive results — negative (404) caching is the in-memory negcache's
// job and is never persisted here.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gocloud.dev/blob"
	"gocloud.dev/gcerrors"

	"github.com/yolocs/open-artifact/pkg/core"
	"github.com/yolocs/open-artifact/pkg/core/blobstore"
)

const (
	cacheDir = ".cache/"
	bodyExt  = ".body"
	metaExt  = ".json"
)

// EntryMeta is the JSON envelope stored alongside a cached body.
type EntryMeta struct {
	Key          string    `json:"key"`
	ContentType  string    `json:"content_type,omitempty"`
	Status       int       `json:"status"`
	FetchedAt    time.Time `json:"fetched_at"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
	ETag         string    `json:"etag,omitempty"`
	LastModified string    `json:"last_modified,omitempty"`
	Digest       string    `json:"digest,omitempty"` // sha256: of body
}

// fresh reports whether the entry is unexpired at now. A zero ExpiresAt means
// fresh forever.
func (m EntryMeta) fresh(now time.Time) bool {
	if m.ExpiresAt.IsZero() {
		return true
	}
	return now.Before(m.ExpiresAt)
}

// Entry is a cached response: its metadata envelope and body. Stale is set by
// GetStale to report that the entry has expired but is still being served (an
// upstream-outage fallback).
type Entry struct {
	Meta  EntryMeta
	Body  []byte
	Stale bool
}

// Cache reads and writes proxy cache entries on a blob bucket.
type Cache struct {
	bucket *blob.Bucket
	prefix string
	now    func() time.Time
}

// Option customizes a Cache.
type Option func(*Cache)

// withClock overrides the time source (tests only).
func withClock(now func() time.Time) Option {
	return func(c *Cache) { c.now = now }
}

// New constructs a Cache over b. bucketPrefix is the optional deployment prefix
// (the same one passed to the namespace registry) inserted after the fixed
// root. The bucket is owned by the caller; the Cache never closes it.
func New(b *blob.Bucket, bucketPrefix string, opts ...Option) (*Cache, error) {
	if b == nil {
		return nil, errors.New("cache: nil bucket")
	}
	c := &Cache{bucket: b, prefix: bucketPrefix, now: time.Now}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// Get returns a fresh cached entry. A missing or expired entry yields
// (zero, false, nil); use GetStale to retrieve an expired entry during an
// upstream outage. A persisted entry whose body no longer matches its recorded
// digest yields core.ErrDigestMismatch.
func (c *Cache) Get(ctx context.Context, ns, format, key string) (Entry, bool, error) {
	e, found, err := c.read(ctx, ns, format, key)
	if err != nil || !found {
		return Entry{}, false, err
	}
	if !e.Meta.fresh(c.now()) {
		return Entry{}, false, nil
	}
	return e, true, nil
}

// GetStale returns a cached entry whether or not it is fresh, setting
// Entry.Stale on an expired one. It is the upstream-outage fallback; a missing
// entry yields (zero, false, nil).
func (c *Cache) GetStale(ctx context.Context, ns, format, key string) (Entry, bool, error) {
	e, found, err := c.read(ctx, ns, format, key)
	if err != nil || !found {
		return Entry{}, false, err
	}
	e.Stale = !e.Meta.fresh(c.now())
	return e, true, nil
}

// Put writes an entry, stamping the original key and a freshly computed body
// digest into the metadata, and defaulting FetchedAt to now when unset. The
// body is written before the metadata so a present metadata object always has
// a body behind it.
func (c *Cache) Put(ctx context.Context, ns, format, key string, entry Entry) error {
	base := c.base(ns, format, key)
	meta := entry.Meta
	meta.Key = key
	meta.Digest = digestOf(entry.Body)
	if meta.FetchedAt.IsZero() {
		meta.FetchedAt = c.now().UTC()
	}

	if err := c.bucket.WriteAll(ctx, base+bodyExt, entry.Body, nil); err != nil {
		return fmt.Errorf("cache: write body %q: %w", key, err)
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("cache: encode meta %q: %w", key, err)
	}
	if err := c.bucket.WriteAll(ctx, base+metaExt, raw, nil); err != nil {
		return fmt.Errorf("cache: write meta %q: %w", key, err)
	}
	return nil
}

// Delete removes an entry's body and metadata. A missing object is not an
// error. It backs tests and admin cleanup.
func (c *Cache) Delete(ctx context.Context, ns, format, key string) error {
	base := c.base(ns, format, key)
	for _, p := range []string{base + bodyExt, base + metaExt} {
		if err := c.bucket.Delete(ctx, p); err != nil && gcerrors.Code(err) != gcerrors.NotFound {
			return fmt.Errorf("cache: delete %q: %w", p, err)
		}
	}
	return nil
}

// read loads an entry's metadata and body. A missing metadata object — or a
// metadata object with no body behind it — is reported as absent, not an error.
// A body that disagrees with the recorded digest yields core.ErrDigestMismatch.
func (c *Cache) read(ctx context.Context, ns, format, key string) (Entry, bool, error) {
	base := c.base(ns, format, key)

	raw, err := c.bucket.ReadAll(ctx, base+metaExt)
	if err != nil {
		if gcerrors.Code(err) == gcerrors.NotFound {
			return Entry{}, false, nil
		}
		return Entry{}, false, fmt.Errorf("cache: read meta %q: %w", key, err)
	}
	var meta EntryMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return Entry{}, false, fmt.Errorf("cache: decode meta %q: %w", key, err)
	}

	body, err := c.bucket.ReadAll(ctx, base+bodyExt)
	if err != nil {
		if gcerrors.Code(err) == gcerrors.NotFound {
			return Entry{}, false, nil
		}
		return Entry{}, false, fmt.Errorf("cache: read body %q: %w", key, err)
	}
	if meta.Digest != "" {
		if got := digestOf(body); got != meta.Digest {
			return Entry{}, false, fmt.Errorf("cache: %q (stored %s, got %s): %w", key, meta.Digest, got, core.ErrDigestMismatch)
		}
	}
	return Entry{Meta: meta, Body: body}, true, nil
}

// base returns the cache path prefix for a key (without the .body/.json
// suffix): open-artifact/v1/<prefix>/<ns>/<fmt>/.cache/<sha256(key)>.
func (c *Cache) base(ns, format, key string) string {
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

// digestOf renders the body's sha256 as "sha256:<hex>", matching the digest
// format core uses elsewhere.
func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}
