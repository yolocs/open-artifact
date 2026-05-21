package namespace

import (
	"context"
	"errors"
	"fmt"

	"gocloud.dev/blob"

	"github.com/yolocs/open-artifact/pkg/core"
	"github.com/yolocs/open-artifact/pkg/core/blobstore"
)

// allowedFormats is the set of data-plane formats a namespace may be scoped
// to. Future formats must be added explicitly.
var allowedFormats = map[string]bool{
	string(core.FormatPyPI):  true,
	string(core.FormatNPM):   true,
	string(core.FormatMaven): true,
}

// Registry is the data-plane namespace factory. It yields namespace-and-format
// scoped core.Store handles and the namespace spec for hosted/proxy dispatch.
// It reads the same bucket as the catalog Store, so admin changes are visible
// without restart.
type Registry struct {
	bucket  *blob.Bucket
	prefix  string
	catalog *Store
}

// NewRegistry constructs a data-plane factory over b. bucketPrefix is the
// optional deployment prefix; catalog provides namespace spec lookups (it must
// be constructed over the same bucket and prefix).
func NewRegistry(b *blob.Bucket, bucketPrefix string, catalog *Store) (*Registry, error) {
	if b == nil {
		return nil, errors.New("namespace: nil bucket")
	}
	if catalog == nil {
		return nil, errors.New("namespace: nil catalog")
	}
	return &Registry{bucket: b, prefix: bucketPrefix, catalog: catalog}, nil
}

// For returns a Scoped handle for a namespace and format. It validates the
// namespace name and format (rejecting path escapes and unknown formats) but
// performs no I/O; namespace existence is observed via Scoped.Spec.
func (r *Registry) For(namespace, format string) (*Scoped, error) {
	if err := ValidateName(namespace); err != nil {
		return nil, err
	}
	if !allowedFormats[format] {
		return nil, fmt.Errorf("namespace: unsupported format %q: want pypi, npm, or maven", format)
	}
	return &Scoped{registry: r, namespace: namespace, format: format}, nil
}

// Scoped binds a namespace and format. It hands out a core.Store rooted at
// scope <ns>/<fmt> and resolves the namespace spec for mode dispatch.
type Scoped struct {
	registry  *Registry
	namespace string
	format    string
}

// Namespace returns the namespace name this handle is bound to.
func (s *Scoped) Namespace() string { return s.namespace }

// Format returns the format this handle is bound to.
func (s *Scoped) Format() string { return s.format }

// Store returns a core.Store bound to scope <prefix>/<ns>/<fmt>. The scope is
// path-safe by construction: namespace names and format names are validated.
func (s *Scoped) Store() (core.Store, error) {
	scope := scopeOf(s.registry.prefix, s.namespace, s.format)
	return blobstore.NewWithBucket(s.registry.bucket, scope)
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
