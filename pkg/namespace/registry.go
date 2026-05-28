package namespace

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gocloud.dev/blob"

	"github.com/yolocs/open-artifact/pkg/core"
	"github.com/yolocs/open-artifact/pkg/core/blobstore"
)

// defaultPolicyCacheTTL is how long a compiled namespace policy (and a negative
// "namespace missing" result) is cached before reloading.
const defaultPolicyCacheTTL = 60 * time.Second

// allowedFormats is the set of data-plane formats a namespace may be scoped
// to. Future formats must be added explicitly.
var allowedFormats = map[string]bool{
	string(core.FormatPyPI):    true,
	string(core.FormatNPM):     true,
	string(core.FormatMaven):   true,
	string(core.FormatDebian):  true,
	string(core.FormatGeneric): true,
}

// Registry is the data-plane namespace factory. It yields namespace-and-format
// scoped core.Store handles and the namespace spec for hosted/proxy dispatch.
// It reads the same bucket as the catalog Store, so admin changes are visible
// without restart.
type Registry struct {
	bucket  *blob.Bucket
	prefix  string
	catalog *Store
	cache   *policyCache
	metrics blobstore.Metrics

	// loadGate is a test seam invoked at the start of a cache miss load,
	// before any I/O. It lets tests prove singleflight collapses concurrent
	// misses. Nil in production.
	loadGate func()
}

// RegistryOption customizes a Registry at construction.
type RegistryOption func(*Registry)

// WithPolicyCacheTTL sets the compiled-policy cache TTL. A non-positive TTL
// disables caching entirely (every Authorized call reloads), which tests use to
// observe uncached behavior.
func WithPolicyCacheTTL(ttl time.Duration) RegistryOption {
	return func(r *Registry) { r.cache.ttl = ttl }
}

// withClock overrides the cache clock (tests only).
func withClock(now func() time.Time) RegistryOption {
	return func(r *Registry) { r.cache.now = now }
}

// WithMetrics installs a blob-backend metrics observer on every scoped
// core.Store the Registry yields, so backend calls made through the data plane
// are instrumented.
func WithMetrics(m blobstore.Metrics) RegistryOption {
	return func(r *Registry) { r.metrics = m }
}

// withLoadGate sets the cache-miss load seam (tests only).
func withLoadGate(fn func()) RegistryOption {
	return func(r *Registry) { r.loadGate = fn }
}

// NewRegistry constructs a data-plane factory over b. bucketPrefix is the
// optional deployment prefix; catalog provides namespace spec lookups (it must
// be constructed over the same bucket and prefix). The Registry registers a
// change hook on catalog so admin Put/Delete invalidate the policy cache
// immediately within a single process.
func NewRegistry(b *blob.Bucket, bucketPrefix string, catalog *Store, opts ...RegistryOption) (*Registry, error) {
	if b == nil {
		return nil, errors.New("namespace: nil bucket")
	}
	if catalog == nil {
		return nil, errors.New("namespace: nil catalog")
	}
	r := &Registry{
		bucket:  b,
		prefix:  bucketPrefix,
		catalog: catalog,
		cache:   newPolicyCache(defaultPolicyCacheTTL),
	}
	for _, opt := range opts {
		opt(r)
	}
	catalog.hooks = append(catalog.hooks, r.cache.invalidate)
	return r, nil
}

// For returns a Scoped handle for a namespace and format. It validates the
// namespace name and format (rejecting path escapes and unknown formats) but
// performs no I/O; namespace existence is observed via Scoped.Spec.
func (r *Registry) For(namespace, format string) (*Scoped, error) {
	if err := ValidateName(namespace); err != nil {
		return nil, err
	}
	if !allowedFormats[format] {
		return nil, fmt.Errorf("namespace: unsupported format %q: want pypi, npm, maven, debian, or generic", format)
	}
	return &Scoped{registry: r, namespace: namespace, format: format}, nil
}

// Scoped binds a namespace and format. It hands out a core.Store rooted at
// scope <ns>/<fmt> and resolves the namespace spec for mode dispatch.
type Scoped struct {
	registry  *Registry
	namespace string
	format    string

	// guard, when set, is installed on the scoped core.Store so every
	// read/write is authorized. It is set by Registry.Authorized; the raw
	// Store() (no guard) is reserved for trusted, internal callers.
	guard blobstore.Guard
}

// Namespace returns the namespace name this handle is bound to.
func (s *Scoped) Namespace() string { return s.namespace }

// Format returns the format this handle is bound to.
func (s *Scoped) Format() string { return s.format }

// Store returns a core.Store bound to scope <prefix>/<ns>/<fmt>. The scope is
// path-safe by construction: namespace names and format names are validated.
func (s *Scoped) Store() (core.Store, error) {
	scope := scopeOf(s.registry.prefix, s.namespace, s.format)
	var opts []blobstore.Option
	if s.guard != nil {
		opts = append(opts, blobstore.WithGuard(s.guard))
	}
	if s.registry.metrics != nil {
		opts = append(opts, blobstore.WithMetrics(s.registry.metrics))
	}
	return blobstore.NewWithBucket(s.registry.bucket, scope, opts...)
}

// Spec returns the namespace spec for mode/proxy dispatch. An unknown
// namespace maps to ErrNotFound.
func (s *Scoped) Spec(ctx context.Context) (Spec, error) {
	ns, err := s.registry.catalog.load(ctx, s.namespace)
	if err != nil {
		return Spec{}, err
	}
	return ns.Spec, nil
}

// scopeOf joins the deployment prefix, namespace, and format into the
// blobstore scope. blobstore prepends the fixed root and a trailing slash.
func scopeOf(prefix, namespace, format string) string {
	scope := namespace + "/" + format
	if prefix != "" {
		scope = prefix + "/" + scope
	}
	return scope
}
