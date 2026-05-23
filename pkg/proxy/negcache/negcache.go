// Package negcache is the proxy's in-memory negative cache for repeated
// upstream 404s. It is process-local and reconstructible — a missing entry just
// means the next request re-checks upstream — so it never touches the bucket.
//
// Entries are keyed by namespace, format, and the logical upstream key, and
// expire after a configurable TTL. The cache is safe for concurrent use.
package negcache

import (
	"sync"
	"time"
)

// DefaultTTL is how long an upstream 404 is remembered when no override is
// given. It is deliberately short: a package that appears upstream should
// become fetchable soon after, not after minutes of cached absence.
const DefaultTTL = 30 * time.Second

// Cache remembers recent upstream 404s.
type Cache struct {
	mu      sync.Mutex
	entries map[string]time.Time // key -> expiry
	ttl     time.Duration
	now     func() time.Time
}

// Option customizes a Cache.
type Option func(*Cache)

// withClock overrides the time source (tests only).
func withClock(now func() time.Time) Option {
	return func(c *Cache) { c.now = now }
}

// New constructs a negative cache with the given TTL. A non-positive ttl falls
// back to DefaultTTL.
func New(ttl time.Duration, opts ...Option) *Cache {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	c := &Cache{
		entries: make(map[string]time.Time),
		ttl:     ttl,
		now:     time.Now,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Mark records that (ns, format, key) was a 404 upstream, remembered until now
// + ttl.
func (c *Cache) Mark(ns, format, key string) {
	k := cacheKey(ns, format, key)
	c.mu.Lock()
	c.entries[k] = c.now().Add(c.ttl)
	c.mu.Unlock()
}

// Has reports whether (ns, format, key) is a still-valid negative entry. An
// expired entry is dropped lazily and reported as absent.
func (c *Cache) Has(ns, format, key string) bool {
	k := cacheKey(ns, format, key)
	c.mu.Lock()
	defer c.mu.Unlock()
	exp, ok := c.entries[k]
	if !ok {
		return false
	}
	if !c.now().Before(exp) {
		delete(c.entries, k)
		return false
	}
	return true
}

// Delete drops any negative entry for (ns, format, key). It is used when a
// later fetch succeeds, or by admin/test cleanup.
func (c *Cache) Delete(ns, format, key string) {
	k := cacheKey(ns, format, key)
	c.mu.Lock()
	delete(c.entries, k)
	c.mu.Unlock()
}

// cacheKey joins the parts with a separator that cannot appear in a namespace
// or format name, so distinct triples never collide.
func cacheKey(ns, format, key string) string {
	return ns + "\x00" + format + "\x00" + key
}
