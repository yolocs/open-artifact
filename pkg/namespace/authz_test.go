package namespace

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gocloud.dev/blob"
	"gocloud.dev/blob/memblob"

	"github.com/yolocs/open-artifact/pkg/auth"
)

func memBucket(t *testing.T) *blob.Bucket {
	t.Helper()
	b := memblob.OpenBucket(nil)
	t.Cleanup(func() { b.Close() })
	return b
}

// readerPolicy allows the given OIDC subject to read; writerPolicy to write.
func readerPolicy(sub string) Policy {
	return Policy{Readers: []SubjectMatcher{{Issuer: "https://idp", SubMatch: sub}}}
}

func putNS(t *testing.T, s *Store, name string, policy Policy) {
	t.Helper()
	if _, err := s.Put(t.Context(), &Namespace{Name: name, Spec: Spec{Policy: policy}}); err != nil {
		t.Fatalf("Put(%q): %v", name, err)
	}
}

func TestAuthorizedCrossNamespaceIsolation(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		store, err := NewStore(b, "")
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		reg, err := NewRegistry(b, "", store)
		if err != nil {
			t.Fatalf("NewRegistry: %v", err)
		}
		putNS(t, store, "team-a", readerPolicy("alice"))
		putNS(t, store, "team-b", readerPolicy("bob"))

		alice := oidcCtx("https://idp", "alice", "", nil)

		// alice may read team-a.
		sa, err := reg.Authorized(ctx, "team-a", "pypi", alice)
		if err != nil {
			t.Fatalf("Authorized(team-a): %v", err)
		}
		if _, err := sa.Packages(ctx); err != nil {
			t.Errorf("alice read team-a = %v, want allowed", err)
		}

		// alice may not read team-b.
		sb, err := reg.Authorized(ctx, "team-b", "pypi", alice)
		if err != nil {
			t.Fatalf("Authorized(team-b): %v", err)
		}
		if _, err := sb.Packages(ctx); !errors.Is(err, auth.ErrUnauthorized) {
			t.Errorf("alice read team-b = %v, want ErrUnauthorized", err)
		}
	})
}

func TestAuthorizedProxyStoreReaderCanFillCache(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	b := memBucket(t)
	store, _ := NewStore(b, "")
	reg, _ := NewRegistry(b, "", store)

	// A proxy namespace with only a reader policy: the reader must be able to
	// populate the cache (write real artifact Files) even though it cannot write
	// in hosted mode.
	if _, err := store.Put(ctx, &Namespace{
		Name: "team-proxy",
		Spec: Spec{Mode: ModeProxy, Proxy: Proxy{Upstream: "https://pypi.org"}, Policy: readerPolicy("alice")},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	alice := oidcCtx("https://idp", "alice", "", nil)

	s, spec, err := reg.AuthorizedProxyStore(ctx, "team-proxy", "pypi", alice)
	if err != nil {
		t.Fatalf("AuthorizedProxyStore: %v", err)
	}
	if !spec.IsProxy() {
		t.Fatalf("spec.IsProxy() = false, want true")
	}
	if _, err := s.AddPackage(ctx, "requests"); err != nil {
		t.Errorf("reader cache fill (AddPackage) = %v, want allowed (unguarded proxy store)", err)
	}

	// A non-reader is denied before any store handle is usable.
	bob := oidcCtx("https://idp", "bob", "", nil)
	if _, _, err := reg.AuthorizedProxyStore(ctx, "team-proxy", "pypi", bob); !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("non-reader AuthorizedProxyStore = %v, want ErrUnauthorized", err)
	}

	// An unknown namespace is ErrNotFound.
	if _, _, err := reg.AuthorizedProxyStore(ctx, "missing", "pypi", alice); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown namespace = %v, want ErrNotFound", err)
	}
}

func TestAuthorizedReadWriteSeparation(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	b := memBucket(t)
	store, _ := NewStore(b, "")
	reg, _ := NewRegistry(b, "", store)

	// Reader-only policy: alice can read but not write.
	putNS(t, store, "team-a", readerPolicy("alice"))
	alice := oidcCtx("https://idp", "alice", "", nil)

	s, err := reg.Authorized(ctx, "team-a", "pypi", alice)
	if err != nil {
		t.Fatalf("Authorized: %v", err)
	}
	if _, err := s.Packages(ctx); err != nil {
		t.Errorf("read = %v, want allowed", err)
	}
	if _, err := s.AddPackage(ctx, "requests"); !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("write = %v, want ErrUnauthorized (write does not follow from read)", err)
	}
}

func TestAuthorizedDenyAll(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	b := memBucket(t)
	store, _ := NewStore(b, "")
	reg, _ := NewRegistry(b, "", store)

	putNS(t, store, "team-a", Policy{}) // empty policy = deny-all
	alice := oidcCtx("https://idp", "alice", "", nil)

	s, err := reg.Authorized(ctx, "team-a", "pypi", alice)
	if err != nil {
		t.Fatalf("Authorized: %v", err)
	}
	if _, err := s.Packages(ctx); !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("read on deny-all = %v, want ErrUnauthorized", err)
	}
	if _, err := s.AddPackage(ctx, "requests"); !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("write on deny-all = %v, want ErrUnauthorized", err)
	}
}

func TestAuthorizedNilContext(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	b := memBucket(t)
	store, _ := NewStore(b, "")
	reg, _ := NewRegistry(b, "", store)

	putNS(t, store, "team-a", readerPolicy("alice"))

	s, err := reg.Authorized(ctx, "team-a", "pypi", nil)
	if err != nil {
		t.Fatalf("Authorized(nil ac): %v", err)
	}
	if _, err := s.Packages(ctx); !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("read with nil ac = %v, want ErrUnauthorized", err)
	}
}

func TestAuthorizedUnknownNamespace(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	b := memBucket(t)
	store, _ := NewStore(b, "")
	reg, _ := NewRegistry(b, "", store)

	alice := oidcCtx("https://idp", "alice", "", nil)
	if _, err := reg.Authorized(ctx, "ghost", "pypi", alice); !errors.Is(err, ErrNotFound) {
		t.Errorf("Authorized(unknown) = %v, want ErrNotFound", err)
	}
}

// clock is a controllable time source for cache tests.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestPolicyCacheTTL(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	b := memBucket(t)
	// Separate read/write catalogs so the OnChange hook does not invalidate the
	// registry cache — this isolates pure TTL behavior, mirroring the
	// cross-process data/admin split.
	readCatalog, _ := NewStore(b, "")
	writeCatalog, _ := NewStore(b, "")
	clk := &clock{t: time.Now()}
	reg, _ := NewRegistry(b, "", readCatalog, withClock(clk.now), WithPolicyCacheTTL(60*time.Second))

	putNS(t, writeCatalog, "team-a", readerPolicy("alice"))
	alice := oidcCtx("https://idp", "alice", "", nil)

	read := func() error {
		s, err := reg.Authorized(ctx, "team-a", "pypi", alice)
		if err != nil {
			return err
		}
		_, err = s.Packages(ctx)
		return err
	}

	if err := read(); err != nil {
		t.Fatalf("initial read = %v, want allowed", err)
	}

	// Revoke alice via the write catalog; the registry keeps serving the cached
	// (allowing) policy until the TTL expires.
	putNS(t, writeCatalog, "team-a", Policy{})
	if err := read(); err != nil {
		t.Errorf("read within TTL = %v, want still-allowed (stale cache)", err)
	}

	clk.advance(61 * time.Second)
	if err := read(); !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("read after TTL = %v, want ErrUnauthorized (reloaded)", err)
	}
}

func TestPolicyCacheDisabled(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	b := memBucket(t)
	readCatalog, _ := NewStore(b, "")
	writeCatalog, _ := NewStore(b, "")
	reg, _ := NewRegistry(b, "", readCatalog, WithPolicyCacheTTL(0))

	putNS(t, writeCatalog, "team-a", readerPolicy("alice"))
	alice := oidcCtx("https://idp", "alice", "", nil)

	read := func() error {
		s, err := reg.Authorized(ctx, "team-a", "pypi", alice)
		if err != nil {
			return err
		}
		_, err = s.Packages(ctx)
		return err
	}

	if err := read(); err != nil {
		t.Fatalf("initial read = %v, want allowed", err)
	}
	putNS(t, writeCatalog, "team-a", Policy{})
	if err := read(); !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("read with caching disabled = %v, want immediate ErrUnauthorized", err)
	}
}

func TestPolicyNegativeCache(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	b := memBucket(t)
	readCatalog, _ := NewStore(b, "")
	writeCatalog, _ := NewStore(b, "")
	clk := &clock{t: time.Now()}
	reg, _ := NewRegistry(b, "", readCatalog, withClock(clk.now), WithPolicyCacheTTL(60*time.Second))

	alice := oidcCtx("https://idp", "alice", "", nil)

	if _, err := reg.Authorized(ctx, "team-a", "pypi", alice); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Authorized(missing) = %v, want ErrNotFound", err)
	}

	// Create it through the write catalog; the negative result stays cached.
	putNS(t, writeCatalog, "team-a", readerPolicy("alice"))
	if _, err := reg.Authorized(ctx, "team-a", "pypi", alice); !errors.Is(err, ErrNotFound) {
		t.Errorf("Authorized(within negative TTL) = %v, want still ErrNotFound", err)
	}

	clk.advance(61 * time.Second)
	s, err := reg.Authorized(ctx, "team-a", "pypi", alice)
	if err != nil {
		t.Fatalf("Authorized(after negative TTL) = %v, want found", err)
	}
	if _, err := s.Packages(ctx); err != nil {
		t.Errorf("read after negative TTL expiry = %v, want allowed", err)
	}
}

func TestPolicyCacheSingleflight(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	b := memBucket(t)
	store, _ := NewStore(b, "")

	var loads atomic.Int64
	release := make(chan struct{})
	gate := func() {
		loads.Add(1)
		<-release
	}
	reg, _ := NewRegistry(b, "", store, withLoadGate(gate))

	putNS(t, store, "team-a", readerPolicy("alice"))
	alice := oidcCtx("https://idp", "alice", "", nil)

	const n = 24
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = reg.Authorized(ctx, "team-a", "pypi", alice)
		}(i)
	}
	close(start) // release all callers simultaneously into a cold cache

	// Exactly one loader enters the gate; the rest coalesce behind singleflight.
	for loads.Load() < 1 {
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait()

	if got := loads.Load(); got != 1 {
		t.Errorf("loads = %d, want 1 (singleflight collapses concurrent misses)", got)
	}
	for i, err := range errs {
		if err != nil {
			t.Errorf("Authorized[%d] = %v, want nil", i, err)
		}
	}
}

func TestPolicyCacheInvalidationOnPutDelete(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	b := memBucket(t)
	// One shared catalog backs both writes and the registry, so admin Put/Delete
	// fire the OnChange hook that invalidates the cache immediately.
	store, _ := NewStore(b, "")
	reg, _ := NewRegistry(b, "", store)

	putNS(t, store, "team-a", readerPolicy("alice"))
	alice := oidcCtx("https://idp", "alice", "", nil)

	read := func() error {
		s, err := reg.Authorized(ctx, "team-a", "pypi", alice)
		if err != nil {
			return err
		}
		_, err = s.Packages(ctx)
		return err
	}

	if err := read(); err != nil {
		t.Fatalf("initial read = %v, want allowed", err)
	}

	// Put invalidates immediately: the revocation takes effect on the next call.
	putNS(t, store, "team-a", Policy{})
	if err := read(); !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("read after Put = %v, want ErrUnauthorized (cache invalidated)", err)
	}

	// Delete invalidates too: the namespace is now unknown.
	if err := store.Delete(ctx, "team-a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := reg.Authorized(ctx, "team-a", "pypi", alice); !errors.Is(err, ErrNotFound) {
		t.Errorf("Authorized after Delete = %v, want ErrNotFound", err)
	}
}
