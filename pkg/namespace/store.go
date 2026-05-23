package namespace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"gocloud.dev/blob"
	"gocloud.dev/gcerrors"

	"github.com/yolocs/open-artifact/pkg/core/blobstore"
	"github.com/yolocs/open-artifact/pkg/logging"
)

// metaName is the namespace metadata object directly under a namespace
// directory. It mirrors the Store-owned ".meta" convention at every level.
const metaName = ".meta"

// Store is the blob-backed namespace catalog. It owns the <ns>/.meta object
// and is its only writer. It is the source of truth for which namespaces exist
// and what their specs are.
type Store struct {
	bucket *blob.Bucket
	// root is the on-bucket prefix under which namespaces live:
	// "open-artifact/v1/" with the optional bucket prefix appended.
	root  string
	hooks []func(name string)
}

// Option customizes a Store at construction.
type Option func(*Store)

// OnChange registers a callback invoked with the namespace name after a
// successful Put or Delete. It backs auth/cache invalidation (#7). Multiple
// callbacks may be registered; they run in registration order.
func OnChange(fn func(name string)) Option {
	return func(s *Store) {
		if fn != nil {
			s.hooks = append(s.hooks, fn)
		}
	}
}

// NewStore constructs a namespace catalog over b. bucketPrefix is the optional
// deployment prefix inserted after the fixed root (already validated by the
// command layer); an empty prefix uses the bare root. The bucket is owned by
// the caller; the Store never closes it.
func NewStore(b *blob.Bucket, bucketPrefix string, opts ...Option) (*Store, error) {
	if b == nil {
		return nil, errors.New("namespace: nil bucket")
	}
	s := &Store{bucket: b, root: rootPrefix(bucketPrefix)}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// rootPrefix returns the on-bucket prefix under which namespaces live.
func rootPrefix(bucketPrefix string) string {
	bucketPrefix = strings.Trim(bucketPrefix, "/")
	if bucketPrefix == "" {
		return blobstore.Root
	}
	return blobstore.Root + bucketPrefix + "/"
}

// nsPrefix returns the directory prefix for a namespace.
func (s *Store) nsPrefix(name string) string { return s.root + name + "/" }

// nsMetaPath returns the path of a namespace's .meta object.
func (s *Store) nsMetaPath(name string) string { return s.nsPrefix(name) + metaName }

// Put creates or updates a namespace. It validates the name and spec,
// normalizes the spec (stamping schema_version, collapsing an explicit hosted
// mode), and writes <ns>/.meta. The returned Namespace reflects the stored,
// normalized form.
func (s *Store) Put(ctx context.Context, ns *Namespace) (*Namespace, error) {
	if ns == nil {
		return nil, errors.New("namespace: nil namespace")
	}
	if err := ValidateName(ns.Name); err != nil {
		return nil, err
	}
	spec, err := normalizeForWrite(ns.Spec)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("namespace: marshal %q: %w", ns.Name, err)
	}
	if err := s.bucket.WriteAll(ctx, s.nsMetaPath(ns.Name), body, nil); err != nil {
		return nil, fmt.Errorf("namespace: write %q: %w", ns.Name, err)
	}
	s.notify(ns.Name)
	return &Namespace{Name: ns.Name, Spec: spec}, nil
}

// Get returns the named namespace. A missing namespace maps to ErrNotFound.
func (s *Store) Get(ctx context.Context, name string) (*Namespace, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	return s.load(ctx, name)
}

// load reads and decodes a namespace's .meta, mapping a missing object to
// ErrNotFound. It does not re-validate the name.
func (s *Store) load(ctx context.Context, name string) (*Namespace, error) {
	raw, err := s.bucket.ReadAll(ctx, s.nsMetaPath(name))
	if err != nil {
		if gcerrors.Code(err) == gcerrors.NotFound {
			return nil, fmt.Errorf("%w: %q", ErrNotFound, name)
		}
		return nil, fmt.Errorf("namespace: read %q: %w", name, err)
	}
	var spec Spec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return nil, fmt.Errorf("namespace: decode %q: %w", name, err)
	}
	spec, err = normalizeForRead(spec)
	if err != nil {
		return nil, err
	}
	return &Namespace{Name: name, Spec: spec}, nil
}

// List returns every namespace sorted by name. A top-level child directory
// without a .meta object is not a namespace and is skipped with a debug log.
func (s *Store) List(ctx context.Context) ([]*Namespace, error) {
	names, err := s.listChildDirs(ctx, s.root)
	if err != nil {
		return nil, err
	}
	logger := logging.FromContext(ctx)
	out := make([]*Namespace, 0, len(names))
	for _, name := range names {
		ns, err := s.load(ctx, name)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				logger.Debug("skipping non-namespace directory", logging.KeyComponent, "namespace", "name", name)
				continue
			}
			return nil, err
		}
		out = append(out, ns)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Ping proves the namespace catalog is reachable by listing the top-level
// child directories (the catalog index) without loading any metadata. It backs
// admin-plane readiness.
func (s *Store) Ping(ctx context.Context) error {
	_, err := s.listChildDirs(ctx, s.root)
	return err
}

// Delete removes an empty namespace's .meta object. It returns ErrNotFound if
// the namespace does not exist and ErrNotEmpty if it still holds package data.
func (s *Store) Delete(ctx context.Context, name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	if _, err := s.load(ctx, name); err != nil {
		return err
	}
	empty, err := s.isEmpty(ctx, name)
	if err != nil {
		return err
	}
	if !empty {
		return fmt.Errorf("%w: %q", ErrNotEmpty, name)
	}
	if err := s.bucket.Delete(ctx, s.nsMetaPath(name)); err != nil {
		return fmt.Errorf("namespace: delete %q: %w", name, err)
	}
	s.notify(name)
	return nil
}

// isEmpty reports whether a namespace holds no package data. Dot-entries
// (.meta, a regenerable .cache) do not count, and a format directory holding
// only its own .cache is still empty. Package data is a non-dot child under
// some non-dot format directory of the namespace.
func (s *Store) isEmpty(ctx context.Context, name string) (bool, error) {
	formats, err := s.listChildDirs(ctx, s.nsPrefix(name))
	if err != nil {
		return false, err
	}
	for _, fmtDir := range formats {
		pkgs, err := s.listChildDirs(ctx, s.nsPrefix(name)+fmtDir+"/")
		if err != nil {
			return false, err
		}
		if len(pkgs) > 0 {
			return false, nil
		}
	}
	return true, nil
}

// listChildDirs lists the immediate, non-dot child directory names under
// prefix using a "/" delimiter, sorted. Dot-entries are dropped at every
// level — one rule, every level.
func (s *Store) listChildDirs(ctx context.Context, prefix string) ([]string, error) {
	iter := s.bucket.List(&blob.ListOptions{Prefix: prefix, Delimiter: "/"})
	var names []string
	for {
		obj, err := iter.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("namespace: list %q: %w", prefix, err)
		}
		if !obj.IsDir {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(obj.Key, prefix), "/")
		if name == "" || strings.HasPrefix(name, ".") {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// notify runs the registered change hooks for name.
func (s *Store) notify(name string) {
	for _, fn := range s.hooks {
		fn(name)
	}
}
