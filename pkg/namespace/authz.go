package namespace

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/yolocs/open-artifact/pkg/auth"
	"github.com/yolocs/open-artifact/pkg/core"
	"github.com/yolocs/open-artifact/pkg/core/blobstore"
)

// Authorized returns a core.Store for a namespace and format whose every
// read/write operation is authorized against the namespace's compiled policy
// before reaching storage. An unknown namespace maps to ErrNotFound; a
// permitted-but-denied operation maps to auth.ErrUnauthorized. The compiled
// policy is served from a per-namespace cache (see WithPolicyCacheTTL).
//
// Authorization is enforced inside the blobstore.Store via a Guard, not by a
// wrapper around it: the scoped Store carries the guard and consults it on
// every operation, so there is no decorating handle to navigate around.
func (r *Registry) Authorized(ctx context.Context, namespace, format string, ac *auth.AuthContext) (core.Store, error) {
	store, _, err := r.AuthorizedStore(ctx, namespace, format, ac)
	return store, err
}

// AuthorizedStore returns a guarded core.Store plus the namespace spec loaded
// from the same cached namespace lookup. Format surfaces use the spec for
// hosted/proxy dispatch without re-reading namespace metadata separately.
func (r *Registry) AuthorizedStore(ctx context.Context, namespace, format string, ac *auth.AuthContext) (core.Store, Spec, error) {
	scoped, err := r.For(namespace, format)
	if err != nil {
		return nil, Spec{}, err
	}
	entry, err := r.cache.get(ctx, namespace, r.load)
	if err != nil {
		return nil, Spec{}, err
	}
	if entry.notFound {
		return nil, Spec{}, fmt.Errorf("%w: %q", ErrNotFound, namespace)
	}
	scoped.guard = guardFor(entry.authorizer, ac)
	store, err := scoped.Store()
	if err != nil {
		return nil, Spec{}, err
	}
	return store, entry.spec, nil
}

// AuthorizedProxyStore returns an unguarded core.Store for proxy pull-through
// plus the namespace spec, after authorizing ac against the namespace's reader
// policy. Pull-through is a read from the client's perspective — clients only
// GET, and the surface populates the cache (both .cache/ metadata and real
// artifact Files) on their behalf — so reader policy gates the whole operation
// and the returned Store is unguarded so cache-fill writes are not rejected as
// OpWrite. An unknown namespace maps to ErrNotFound; a subject that is not a
// reader maps to auth.ErrUnauthorized.
func (r *Registry) AuthorizedProxyStore(ctx context.Context, namespace, format string, ac *auth.AuthContext) (core.Store, Spec, error) {
	scoped, err := r.For(namespace, format)
	if err != nil {
		return nil, Spec{}, err
	}
	entry, err := r.cache.get(ctx, namespace, r.load)
	if err != nil {
		return nil, Spec{}, err
	}
	if entry.notFound {
		return nil, Spec{}, fmt.Errorf("%w: %q", ErrNotFound, namespace)
	}
	if err := entry.authorizer.Authorize(ctx, ac, auth.OpRead); err != nil {
		return nil, Spec{}, err
	}
	store, err := scoped.Store()
	if err != nil {
		return nil, Spec{}, err
	}
	return store, entry.spec, nil
}

// guardFor adapts a compiled policy and subject to the blobstore.Guard hook:
// reads authorize OpRead, writes OpWrite.
func guardFor(authorizer *compiledPolicy, ac *auth.AuthContext) blobstore.Guard {
	return func(ctx context.Context, write bool) error {
		op := auth.OpRead
		if write {
			op = auth.OpWrite
		}
		return authorizer.Authorize(ctx, ac, op)
	}
}

// load reads a namespace and compiles its policy. A missing namespace is cached
// as a negative entry; a real I/O or compile error is propagated (and not
// cached).
func (r *Registry) load(ctx context.Context, name string) (*policyEntry, error) {
	if r.loadGate != nil {
		r.loadGate()
	}
	ns, err := r.catalog.load(ctx, name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return &policyEntry{notFound: true}, nil
		}
		return nil, err
	}
	compiled, err := compilePolicy(ns.Spec.Policy)
	if err != nil {
		return nil, err
	}
	return &policyEntry{authorizer: compiled, spec: ns.Spec}, nil
}

// policyEntry is a cached lookup: either a compiled authorizer for an existing
// namespace, or a negative result for a missing one.
type policyEntry struct {
	authorizer *compiledPolicy
	spec       Spec
	notFound   bool
	expires    time.Time
}

// policyCache caches policyEntry per namespace for ttl, collapsing concurrent
// misses with singleflight. A non-positive ttl disables caching.
type policyCache struct {
	mu      sync.Mutex
	entries map[string]*policyEntry
	ttl     time.Duration
	now     func() time.Time
	sf      singleflight.Group
}

func newPolicyCache(ttl time.Duration) *policyCache {
	return &policyCache{
		entries: make(map[string]*policyEntry),
		ttl:     ttl,
		now:     time.Now,
	}
}

// get returns the cached entry for name, loading it on a miss. Concurrent
// misses for the same name share one load. With caching disabled it loads every
// time.
func (c *policyCache) get(ctx context.Context, name string, load func(context.Context, string) (*policyEntry, error)) (*policyEntry, error) {
	if c.ttl <= 0 {
		return load(ctx, name)
	}
	if e := c.fresh(name); e != nil {
		return e, nil
	}
	v, err, _ := c.sf.Do(name, func() (any, error) {
		if e := c.fresh(name); e != nil {
			return e, nil
		}
		e, err := load(ctx, name)
		if err != nil {
			return nil, err
		}
		e.expires = c.now().Add(c.ttl)
		c.mu.Lock()
		c.entries[name] = e
		c.mu.Unlock()
		return e, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*policyEntry), nil
}

// fresh returns the cached entry for name if present and unexpired.
func (c *policyCache) fresh(name string) *policyEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[name]; ok && c.now().Before(e.expires) {
		return e
	}
	return nil
}

// invalidate drops the cached entry for name. It is the catalog OnChange hook.
func (c *policyCache) invalidate(name string) {
	c.mu.Lock()
	delete(c.entries, name)
	c.mu.Unlock()
}
