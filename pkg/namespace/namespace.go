// Package namespace defines namespace records and lookup contracts shared by
// admin code, authz wrappers, and protocol surfaces.
package namespace

import (
	"context"
	"errors"
	"net/url"
	"path"
	"strings"
	"sync"

	"github.com/yolocs/open-artifact/pkg/core"
)

var (
	ErrInvalidName              = errors.New("open-artifact namespace: invalid name")
	ErrInvalidProxy             = errors.New("open-artifact namespace: invalid proxy")
	ErrUnsupportedSchemaVersion = errors.New("open-artifact namespace: unsupported schema version")
	ErrNotFound                 = errors.New("open-artifact namespace: not found")
	ErrNotEmpty                 = errors.New("open-artifact namespace: not empty")
)

const (
	KindHosted = "hosted"
	KindProxy  = "proxy"
)

type SubjectMatcher struct {
	Issuer      string            `json:"issuer,omitempty"`
	ID          string            `json:"id,omitempty"`
	SubMatch    string            `json:"sub_match,omitempty"`
	Email       string            `json:"email,omitempty"`
	ClaimsMatch map[string]string `json:"claims_match,omitempty"`
	Kind        string            `json:"kind,omitempty"`
}

type Policy struct {
	Readers []SubjectMatcher `json:"readers,omitempty"`
	Writers []SubjectMatcher `json:"writers,omitempty"`
}

type Spec struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	UpstreamURL string `json:"upstream_url,omitempty"`
	Policy      Policy `json:"policy,omitempty"`
}

type Registry struct {
	mu     sync.RWMutex
	specs  map[string]Spec
	stores map[string]core.Store
}

func NewRegistry() *Registry {
	return &Registry{
		specs:  make(map[string]Spec),
		stores: make(map[string]core.Store),
	}
}

func (r *Registry) Put(ctx context.Context, spec Spec) error {
	_ = ctx
	if err := ValidateName(spec.Name); err != nil {
		return err
	}
	if spec.Kind == "" {
		spec.Kind = KindHosted
	}
	if spec.Kind == KindProxy {
		if spec.UpstreamURL == "" {
			return ErrInvalidProxy
		}
		u, err := url.Parse(spec.UpstreamURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return ErrInvalidProxy
		}
	}
	if spec.Kind != KindHosted && spec.Kind != KindProxy {
		return ErrInvalidName
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.specs[spec.Name] = spec
	return nil
}

func (r *Registry) PutStore(ctx context.Context, name string, store core.Store) error {
	_ = ctx
	if err := ValidateName(name); err != nil {
		return err
	}
	if store == nil {
		return ErrNotFound
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.specs[name]; !ok {
		r.specs[name] = Spec{Name: name, Kind: KindHosted}
	}
	r.stores[name] = store
	return nil
}

func (r *Registry) Spec(ctx context.Context, name string) (Spec, error) {
	_ = ctx
	if err := ValidateName(name); err != nil {
		return Spec{}, err
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	spec, ok := r.specs[name]
	if !ok {
		return Spec{}, ErrNotFound
	}
	return spec, nil
}

func (r *Registry) Resolve(ctx context.Context, name string) (core.Store, error) {
	_ = ctx
	if err := ValidateName(name); err != nil {
		return nil, err
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	store, ok := r.stores[name]
	if !ok {
		return nil, ErrNotFound
	}
	return store, nil
}

func (r *Registry) Delete(ctx context.Context, name string) error {
	_ = ctx
	if err := ValidateName(name); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.specs, name)
	delete(r.stores, name)
	return nil
}

func ValidateName(name string) error {
	if name == "" || strings.HasPrefix(name, "/") || path.Clean(name) != name {
		return ErrInvalidName
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." || strings.HasPrefix(part, ".") {
			return ErrInvalidName
		}
	}
	return nil
}
