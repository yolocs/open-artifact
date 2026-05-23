package blobstore

import (
	"container/list"
	"context"
	"fmt"
	"sync"
	"time"

	"gocloud.dev/blob"
)

const (
	// defaultURLCacheTTL is the default lifetime of a cached signed download
	// URL. It mirrors the ocifactory lineage's ~19-minute blob URL TTL,
	// comfortably under the per-cloud signing maximums.
	defaultURLCacheTTL = 19 * time.Minute
	// defaultStatCacheTTL is the default lifetime of a cached blob attribute
	// descriptor. Kept short so HEAD short-circuits observe fresh sizes.
	defaultStatCacheTTL = time.Minute
	// defaultURLCacheCap and defaultStatCacheCap bound the two LRUs by entry
	// count. A non-positive capacity disables the bound.
	defaultURLCacheCap  = 4096
	defaultStatCacheCap = 4096
)

// signFunc signs a download URL for a blob key. It abstracts
// (*blob.Bucket).SignedURL so tests can count or stub invocations.
type signFunc func(ctx context.Context, key string, opts *blob.SignedURLOptions) (string, error)

// statFunc fetches a blob's attributes. It abstracts
// (*blob.Bucket).Attributes for the same reason.
type statFunc func(ctx context.Context, key string) (*blob.Attributes, error)

// WithURLCacheTTL overrides the lifetime of cached signed download URLs. The
// same value is passed as the SignedURL expiry, so the cached entry never
// outlives the URL it holds.
func WithURLCacheTTL(ttl time.Duration) Option {
	return func(s *Store) { s.urlTTL = ttl }
}

// WithStatCacheTTL overrides the lifetime of cached blob attribute
// descriptors.
func WithStatCacheTTL(ttl time.Duration) Option {
	return func(s *Store) { s.statTTL = ttl }
}

// clampCacheTTL returns the lifetime to cache a signed URL for, given the
// SignedURL expiry was requested for ttl. It shaves a margin (10%, capped at
// one minute) so the cached entry never outlives the URL it holds.
func clampCacheTTL(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return ttl
	}
	margin := ttl / 10
	if margin > time.Minute {
		margin = time.Minute
	}
	return ttl - margin
}

// withSigner overrides the URL signer (used by tests to count invocations).
func withSigner(fn signFunc) Option { return func(s *Store) { s.sign = fn } }

// withStatter overrides the attribute fetcher (used by tests).
func withStatter(fn statFunc) Option { return func(s *Store) { s.stat = fn } }

// attributes returns a blob's attributes through the facade-transparent stat
// cache: an LRU fronted by singleflight so duplicate lookups collapse into a
// single backend call.
func (s *Store) attributes(ctx context.Context, key string) (*blob.Attributes, error) {
	return s.statCache.getOrCompute(key, func() (*blob.Attributes, error) {
		start := time.Now()
		a, err := s.stat(ctx, key)
		s.observe(opAttributes, start, err)
		if err != nil {
			return nil, fmt.Errorf("blobstore: attributes %q: %w", key, mapErr(err))
		}
		return a, nil
	})
}

// flightGroup collapses concurrent calls for the same key into one execution,
// a minimal singleflight that needs no external dependency.
type flightGroup[V any] struct {
	mu sync.Mutex
	m  map[string]*flightCall[V]
}

type flightCall[V any] struct {
	wg  sync.WaitGroup
	val V
	err error
}

// Do runs fn (or waits for an in-flight fn) for key, returning its result to
// every caller that shared the flight.
func (g *flightGroup[V]) Do(key string, fn func() (V, error)) (V, error) {
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*flightCall[V])
	}
	if c, ok := g.m[key]; ok {
		g.mu.Unlock()
		c.wg.Wait()
		return c.val, c.err
	}
	c := &flightCall[V]{}
	c.wg.Add(1)
	g.m[key] = c
	g.mu.Unlock()

	c.val, c.err = fn()
	c.wg.Done()

	g.mu.Lock()
	if g.m[key] == c {
		delete(g.m, key)
	}
	g.mu.Unlock()
	return c.val, c.err
}

// memoCache is a capacity-bounded, TTL-expiring LRU fronted by singleflight.
// It is the shared shape behind both the signed-URL and stat caches: a get
// either hits a live entry or computes one exactly once across racing callers.
type memoCache[V any] struct {
	mu    sync.Mutex
	ll    *list.List
	items map[string]*list.Element
	cap   int
	ttl   time.Duration
	now   func() time.Time

	flight flightGroup[V]
}

type cacheEntry[V any] struct {
	key    string
	val    V
	expiry time.Time
}

func newMemoCache[V any](capacity int, ttl time.Duration, now func() time.Time) *memoCache[V] {
	return &memoCache[V]{
		ll:    list.New(),
		items: make(map[string]*list.Element),
		cap:   capacity,
		ttl:   ttl,
		now:   now,
	}
}

// peek returns a live cached value, evicting and missing on expiry.
func (c *memoCache[V]) peek(key string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		var zero V
		return zero, false
	}
	ent := el.Value.(*cacheEntry[V])
	if c.now().After(ent.expiry) {
		c.ll.Remove(el)
		delete(c.items, key)
		var zero V
		return zero, false
	}
	c.ll.MoveToFront(el)
	return ent.val, true
}

// store inserts or refreshes an entry, evicting the least-recently-used entry
// when over capacity.
func (c *memoCache[V]) store(key string, val V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	expiry := c.now().Add(c.ttl)
	if el, ok := c.items[key]; ok {
		ent := el.Value.(*cacheEntry[V])
		ent.val = val
		ent.expiry = expiry
		c.ll.MoveToFront(el)
		return
	}
	el := c.ll.PushFront(&cacheEntry[V]{key: key, val: val, expiry: expiry})
	c.items[key] = el
	for c.cap > 0 && c.ll.Len() > c.cap {
		oldest := c.ll.Back()
		if oldest == nil {
			break
		}
		c.ll.Remove(oldest)
		delete(c.items, oldest.Value.(*cacheEntry[V]).key)
	}
}

// getOrCompute returns the cached value for key or computes it via fn under
// singleflight. Successful results (including zero values, such as the empty
// URL recorded for backends without redirect support) are cached; errors are
// not.
func (c *memoCache[V]) getOrCompute(key string, fn func() (V, error)) (V, error) {
	if v, ok := c.peek(key); ok {
		return v, nil
	}
	return c.flight.Do(key, func() (V, error) {
		if v, ok := c.peek(key); ok {
			return v, nil
		}
		v, err := fn()
		if err != nil {
			var zero V
			return zero, err
		}
		c.store(key, v)
		return v, nil
	})
}
