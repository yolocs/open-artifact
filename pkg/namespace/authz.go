package namespace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/yolocs/open-artifact/pkg/auth"
	"github.com/yolocs/open-artifact/pkg/core"
)

// Authorized returns a core.Store for a namespace and format whose every
// read/write operation is authorized against the namespace's compiled policy
// before reaching storage. An unknown namespace maps to ErrNotFound; a
// permitted-but-denied operation maps to auth.ErrUnauthorized. The compiled
// policy is served from a per-namespace cache (see WithPolicyCacheTTL).
func (r *Registry) Authorized(ctx context.Context, namespace, format string, ac *auth.AuthContext) (core.Store, error) {
	scoped, err := r.For(namespace, format)
	if err != nil {
		return nil, err
	}
	entry, err := r.cache.get(ctx, namespace, r.load)
	if err != nil {
		return nil, err
	}
	if entry.notFound {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, namespace)
	}
	base, err := scoped.Store()
	if err != nil {
		return nil, err
	}
	return &authStore{guard: guard{authz: entry.authorizer, ac: ac}, inner: base}, nil
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
	return &policyEntry{spec: ns.Spec, authorizer: compiled}, nil
}

// policyEntry is a cached lookup: either a compiled authorizer for an existing
// namespace, or a negative result for a missing one.
type policyEntry struct {
	spec       Spec
	authorizer *compiledPolicy
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

// guard authorizes operations for one subject against one compiled policy. It
// is threaded unchanged through every wrapped noun handle so authz cannot be
// bypassed by navigating the handle graph.
type guard struct {
	authz auth.Authorizer
	ac    *auth.AuthContext
}

func (g guard) check(ctx context.Context, op auth.Op) error {
	return g.authz.Authorize(ctx, g.ac, op)
}

// authStore wraps a core.Store, authorizing each read/write before delegating.
type authStore struct {
	guard
	inner core.Store
}

func (s *authStore) Namespace() string { return s.inner.Namespace() }

func (s *authStore) Package(name string) core.Package {
	return &authPackage{guard: s.guard, inner: s.inner.Package(name)}
}

func (s *authStore) Packages(ctx context.Context) ([]core.Package, error) {
	if err := s.check(ctx, auth.OpRead); err != nil {
		return nil, err
	}
	pkgs, err := s.inner.Packages(ctx)
	if err != nil {
		return nil, err
	}
	return wrapPackages(s.guard, pkgs), nil
}

func (s *authStore) AddPackage(ctx context.Context, name string, opts ...core.CreateOption) (core.Package, error) {
	if err := s.check(ctx, auth.OpWrite); err != nil {
		return nil, err
	}
	p, err := s.inner.AddPackage(ctx, name, opts...)
	if err != nil {
		return nil, err
	}
	return &authPackage{guard: s.guard, inner: p}, nil
}

// authPackage wraps a core.Package.
type authPackage struct {
	guard
	inner core.Package
}

func (p *authPackage) Name() string      { return p.inner.Name() }
func (p *authPackage) Namespace() string { return p.inner.Namespace() }
func (p *authPackage) Store() core.Store { return &authStore{guard: p.guard, inner: p.inner.Store()} }

func (p *authPackage) Meta(ctx context.Context) (core.Meta, error) {
	if err := p.check(ctx, auth.OpRead); err != nil {
		return core.Meta{}, err
	}
	return p.inner.Meta(ctx)
}

func (p *authPackage) Exists(ctx context.Context) (bool, error) {
	if err := p.check(ctx, auth.OpRead); err != nil {
		return false, err
	}
	return p.inner.Exists(ctx)
}

func (p *authPackage) Annotate(ctx context.Context, annotations map[string]any) error {
	if err := p.check(ctx, auth.OpWrite); err != nil {
		return err
	}
	return p.inner.Annotate(ctx, annotations)
}

func (p *authPackage) Version(name string) core.Version {
	return &authVersion{guard: p.guard, inner: p.inner.Version(name)}
}

func (p *authPackage) Versions(ctx context.Context) ([]core.Version, error) {
	if err := p.check(ctx, auth.OpRead); err != nil {
		return nil, err
	}
	vs, err := p.inner.Versions(ctx)
	if err != nil {
		return nil, err
	}
	return wrapVersions(p.guard, vs), nil
}

func (p *authPackage) AddVersion(ctx context.Context, name string, opts ...core.CreateOption) (core.Version, error) {
	if err := p.check(ctx, auth.OpWrite); err != nil {
		return nil, err
	}
	v, err := p.inner.AddVersion(ctx, name, opts...)
	if err != nil {
		return nil, err
	}
	return &authVersion{guard: p.guard, inner: v}, nil
}

func (p *authPackage) Tag(name string) core.Tag {
	return &authTag{guard: p.guard, inner: p.inner.Tag(name)}
}

func (p *authPackage) Tags(ctx context.Context) ([]core.Tag, error) {
	if err := p.check(ctx, auth.OpRead); err != nil {
		return nil, err
	}
	ts, err := p.inner.Tags(ctx)
	if err != nil {
		return nil, err
	}
	return wrapTags(p.guard, ts), nil
}

func (p *authPackage) SetTag(ctx context.Context, name, target string) error {
	if err := p.check(ctx, auth.OpWrite); err != nil {
		return err
	}
	return p.inner.SetTag(ctx, name, target)
}

// authVersion wraps a core.Version.
type authVersion struct {
	guard
	inner core.Version
}

func (v *authVersion) Name() string      { return v.inner.Name() }
func (v *authVersion) Namespace() string { return v.inner.Namespace() }
func (v *authVersion) Package() core.Package {
	return &authPackage{guard: v.guard, inner: v.inner.Package()}
}

func (v *authVersion) Meta(ctx context.Context) (core.Meta, error) {
	if err := v.check(ctx, auth.OpRead); err != nil {
		return core.Meta{}, err
	}
	return v.inner.Meta(ctx)
}

func (v *authVersion) Exists(ctx context.Context) (bool, error) {
	if err := v.check(ctx, auth.OpRead); err != nil {
		return false, err
	}
	return v.inner.Exists(ctx)
}

func (v *authVersion) Annotate(ctx context.Context, annotations map[string]any) error {
	if err := v.check(ctx, auth.OpWrite); err != nil {
		return err
	}
	return v.inner.Annotate(ctx, annotations)
}

func (v *authVersion) File(name string) core.File {
	return &authFile{guard: v.guard, inner: v.inner.File(name)}
}

func (v *authVersion) Files(ctx context.Context) ([]core.File, error) {
	if err := v.check(ctx, auth.OpRead); err != nil {
		return nil, err
	}
	fs, err := v.inner.Files(ctx)
	if err != nil {
		return nil, err
	}
	return wrapFiles(v.guard, fs), nil
}

func (v *authVersion) AddFile(ctx context.Context, name string, body io.Reader, opts ...core.CreateOption) (core.File, error) {
	if err := v.check(ctx, auth.OpWrite); err != nil {
		return nil, err
	}
	f, err := v.inner.AddFile(ctx, name, body, opts...)
	if err != nil {
		return nil, err
	}
	return &authFile{guard: v.guard, inner: f}, nil
}

// authFile wraps a core.File.
type authFile struct {
	guard
	inner core.File
}

func (f *authFile) Name() string      { return f.inner.Name() }
func (f *authFile) Namespace() string { return f.inner.Namespace() }
func (f *authFile) Version() core.Version {
	return &authVersion{guard: f.guard, inner: f.inner.Version()}
}
func (f *authFile) Package() core.Package {
	return &authPackage{guard: f.guard, inner: f.inner.Package()}
}

func (f *authFile) Meta(ctx context.Context) (core.Meta, error) {
	if err := f.check(ctx, auth.OpRead); err != nil {
		return core.Meta{}, err
	}
	return f.inner.Meta(ctx)
}

func (f *authFile) Exists(ctx context.Context) (bool, error) {
	if err := f.check(ctx, auth.OpRead); err != nil {
		return false, err
	}
	return f.inner.Exists(ctx)
}

func (f *authFile) Read(ctx context.Context) (io.ReadCloser, error) {
	if err := f.check(ctx, auth.OpRead); err != nil {
		return nil, err
	}
	return f.inner.Read(ctx)
}

func (f *authFile) DownloadURL(ctx context.Context) (string, error) {
	if err := f.check(ctx, auth.OpRead); err != nil {
		return "", err
	}
	return f.inner.DownloadURL(ctx)
}

// authTag wraps a core.Tag.
type authTag struct {
	guard
	inner core.Tag
}

func (t *authTag) Name() string      { return t.inner.Name() }
func (t *authTag) Namespace() string { return t.inner.Namespace() }
func (t *authTag) Package() core.Package {
	return &authPackage{guard: t.guard, inner: t.inner.Package()}
}

func (t *authTag) Ref(ctx context.Context) (core.Version, error) {
	if err := t.check(ctx, auth.OpRead); err != nil {
		return nil, err
	}
	v, err := t.inner.Ref(ctx)
	if err != nil {
		return nil, err
	}
	return &authVersion{guard: t.guard, inner: v}, nil
}

func (t *authTag) Exists(ctx context.Context) (bool, error) {
	if err := t.check(ctx, auth.OpRead); err != nil {
		return false, err
	}
	return t.inner.Exists(ctx)
}

func wrapPackages(g guard, in []core.Package) []core.Package {
	out := make([]core.Package, len(in))
	for i, p := range in {
		out[i] = &authPackage{guard: g, inner: p}
	}
	return out
}

func wrapVersions(g guard, in []core.Version) []core.Version {
	out := make([]core.Version, len(in))
	for i, v := range in {
		out[i] = &authVersion{guard: g, inner: v}
	}
	return out
}

func wrapFiles(g guard, in []core.File) []core.File {
	out := make([]core.File, len(in))
	for i, f := range in {
		out[i] = &authFile{guard: g, inner: f}
	}
	return out
}

func wrapTags(g guard, in []core.Tag) []core.Tag {
	out := make([]core.Tag, len(in))
	for i, t := range in {
		out[i] = &authTag{guard: g, inner: t}
	}
	return out
}
