//go:build integration

package namespace

import (
	"bytes"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"gocloud.dev/blob"
	"gocloud.dev/blob/fileblob"
	"gocloud.dev/blob/memblob"

	"github.com/google/go-cmp/cmp"
	"github.com/yolocs/open-artifact/pkg/core/blobstore"
)

// backend pairs a name with a factory that opens a fresh bucket.
type backend struct {
	name string
	open func(t *testing.T) *blob.Bucket
}

func backends() []backend {
	return []backend{
		{
			name: "memblob",
			open: func(t *testing.T) *blob.Bucket {
				b := memblob.OpenBucket(nil)
				t.Cleanup(func() { b.Close() })
				return b
			},
		},
		{
			name: "fileblob",
			open: func(t *testing.T) *blob.Bucket {
				b, err := fileblob.OpenBucket(t.TempDir(), nil)
				if err != nil {
					t.Fatalf("fileblob.OpenBucket: %v", err)
				}
				t.Cleanup(func() { b.Close() })
				return b
			},
		},
	}
}

// eachBackend runs fn against every backend as a parallel subtest.
func eachBackend(t *testing.T, fn func(t *testing.T, b *blob.Bucket)) {
	t.Helper()
	for _, be := range backends() {
		be := be
		t.Run(be.name, func(t *testing.T) {
			t.Parallel()
			fn(t, be.open(t))
		})
	}
}

func TestStorePutGetRoundTrip(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		s, err := NewStore(b, "")
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}

		in := &Namespace{
			Name: "team-a",
			Spec: Spec{
				Mode:   ModeProxy,
				Proxy:  Proxy{Upstream: "https://pypi.org/simple"},
				Format: map[string]any{"ttl": "10m"},
			},
		}
		put, err := s.Put(ctx, in)
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		if put.Spec.SchemaVersion != CurrentSchemaVersion {
			t.Errorf("Put stamped version = %d, want %d", put.Spec.SchemaVersion, CurrentSchemaVersion)
		}

		got, err := s.Get(ctx, "team-a")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		want := &Namespace{
			Name: "team-a",
			Spec: Spec{
				SchemaVersion: 1,
				Mode:          ModeProxy,
				Proxy:         Proxy{Upstream: "https://pypi.org/simple"},
				Format:        map[string]any{"ttl": "10m"},
			},
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("Get mismatch (-want +got):\n%s", diff)
		}
	})
}

func TestStoreHostedStoredCompact(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		s, err := NewStore(b, "")
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		if _, err := s.Put(ctx, &Namespace{Name: "hosted-ns", Spec: Spec{Mode: ModeHosted}}); err != nil {
			t.Fatalf("Put: %v", err)
		}

		raw, err := b.ReadAll(ctx, blobstore.Root+"hosted-ns/.meta")
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		var stored map[string]any
		if err := json.Unmarshal(raw, &stored); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if _, ok := stored["mode"]; ok {
			t.Errorf("hosted namespace stored a mode field: %s", raw)
		}
		if v, _ := stored["schema_version"].(float64); v != 1 {
			t.Errorf("stored schema_version = %v, want 1", stored["schema_version"])
		}
	})
}

func TestStoreGetMissing(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		s, err := NewStore(b, "")
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		_, err = s.Get(ctx, "ghost")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get(missing) error = %v, want ErrNotFound", err)
		}
	})
}

func TestStoreListSorted(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		s, err := NewStore(b, "")
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		for _, name := range []string{"zeta", "alpha", "mid"} {
			if _, err := s.Put(ctx, &Namespace{Name: name}); err != nil {
				t.Fatalf("Put(%q): %v", name, err)
			}
		}

		got, err := s.List(ctx)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		var names []string
		for _, ns := range got {
			names = append(names, ns.Name)
		}
		want := []string{"alpha", "mid", "zeta"}
		if diff := cmp.Diff(want, names); diff != "" {
			t.Errorf("List order mismatch (-want +got):\n%s", diff)
		}
	})
}

func TestStoreListSkipsNonNamespaceDirs(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		s, err := NewStore(b, "")
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		if _, err := s.Put(ctx, &Namespace{Name: "real"}); err != nil {
			t.Fatalf("Put: %v", err)
		}
		// A directory with package data but no .meta is not a namespace.
		if err := b.WriteAll(ctx, blobstore.Root+"orphan/pypi/pkg/1.0/file", []byte("x"), nil); err != nil {
			t.Fatalf("WriteAll: %v", err)
		}

		got, err := s.List(ctx)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 1 || got[0].Name != "real" {
			t.Fatalf("List = %+v, want only [real]", got)
		}
	})
}

func TestStoreCustomBucketPrefix(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		s, err := NewStore(b, "shared/dep")
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		if _, err := s.Put(ctx, &Namespace{Name: "team-a"}); err != nil {
			t.Fatalf("Put: %v", err)
		}

		// The object lands under the prefix.
		if ok, _ := b.Exists(ctx, blobstore.Root+"shared/dep/team-a/.meta"); !ok {
			t.Errorf("expected .meta under prefixed path")
		}
		// And is readable through the prefixed Store.
		if _, err := s.Get(ctx, "team-a"); err != nil {
			t.Errorf("Get through prefixed store: %v", err)
		}
	})
}

func TestStoreOnChangeHook(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		var mu sync.Mutex
		var changed []string
		s, err := NewStore(b, "", OnChange(func(name string) {
			mu.Lock()
			defer mu.Unlock()
			changed = append(changed, name)
		}))
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		if _, err := s.Put(ctx, &Namespace{Name: "team-a"}); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if err := s.Delete(ctx, "team-a"); err != nil {
			t.Fatalf("Delete: %v", err)
		}

		mu.Lock()
		defer mu.Unlock()
		want := []string{"team-a", "team-a"}
		if diff := cmp.Diff(want, changed); diff != "" {
			t.Errorf("OnChange calls mismatch (-want +got):\n%s", diff)
		}
	})
}

func TestStoreUnsupportedSchemaOnRead(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		s, err := NewStore(b, "")
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		// A future binary wrote a newer schema; this binary must refuse it.
		raw, _ := json.Marshal(Spec{SchemaVersion: 99})
		if err := b.WriteAll(ctx, blobstore.Root+"future/.meta", raw, nil); err != nil {
			t.Fatalf("WriteAll: %v", err)
		}
		_, err = s.Get(ctx, "future")
		if !errors.Is(err, ErrUnsupportedSchemaVersion) {
			t.Fatalf("Get(future) error = %v, want ErrUnsupportedSchemaVersion", err)
		}
	})
}

func TestStoreDeleteEmpty(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		s, err := NewStore(b, "")
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		if _, err := s.Put(ctx, &Namespace{Name: "team-a"}); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if err := s.Delete(ctx, "team-a"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if ok, _ := b.Exists(ctx, blobstore.Root+"team-a/.meta"); ok {
			t.Errorf(".meta still present after delete")
		}
		if _, err := s.Get(ctx, "team-a"); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get after delete = %v, want ErrNotFound", err)
		}
	})
}

func TestStoreDeleteMissing(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		s, err := NewStore(b, "")
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		if err := s.Delete(ctx, "ghost"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Delete(missing) = %v, want ErrNotFound", err)
		}
	})
}

func TestStoreDeleteNonEmptyConflict(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		s, err := NewStore(b, "")
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		if _, err := s.Put(ctx, &Namespace{Name: "team-a"}); err != nil {
			t.Fatalf("Put: %v", err)
		}
		// Real package data through the data-plane store.
		reg, err := NewRegistry(b, "", s)
		if err != nil {
			t.Fatalf("NewRegistry: %v", err)
		}
		scoped, err := reg.For("team-a", "pypi")
		if err != nil {
			t.Fatalf("For: %v", err)
		}
		ds, err := scoped.Store()
		if err != nil {
			t.Fatalf("Store: %v", err)
		}
		if _, err := ds.Package("requests").Version("2.31.0").AddFile(ctx, "requests-2.31.0.whl", bytes.NewReader([]byte("x"))); err != nil {
			t.Fatalf("AddFile: %v", err)
		}

		if err := s.Delete(ctx, "team-a"); !errors.Is(err, ErrNotEmpty) {
			t.Fatalf("Delete(non-empty) = %v, want ErrNotEmpty", err)
		}
	})
}

func TestStoreDeleteCacheOnlyIsEmpty(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		s, err := NewStore(b, "")
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		if _, err := s.Put(ctx, &Namespace{Name: "team-a"}); err != nil {
			t.Fatalf("Put: %v", err)
		}
		// Regenerable caches at namespace and format level do not count as data.
		if err := b.WriteAll(ctx, blobstore.Root+"team-a/.cache/blob", []byte("x"), nil); err != nil {
			t.Fatalf("WriteAll: %v", err)
		}
		if err := b.WriteAll(ctx, blobstore.Root+"team-a/pypi/.cache/blob", []byte("x"), nil); err != nil {
			t.Fatalf("WriteAll: %v", err)
		}

		if err := s.Delete(ctx, "team-a"); err != nil {
			t.Fatalf("Delete(cache-only) = %v, want nil", err)
		}
	})
}
