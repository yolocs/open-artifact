//go:build integration

package cache

import (
	"testing"

	"gocloud.dev/blob"
	"gocloud.dev/blob/fileblob"
	"gocloud.dev/blob/memblob"

	"github.com/yolocs/open-artifact/pkg/core/blobstore"
)

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

// TestPersistsAcrossInstances proves an entry written by one Cache is readable
// by a fresh Cache over the same bucket — i.e. the cache is durable bucket
// state, not in-process memory.
func TestPersistsAcrossInstances(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		const ns, format, key = "team-a", "npm", "npm:packument:@scope/name"
		body := []byte(`{"name":"@scope/name"}`)

		writer, err := New(b, "")
		if err != nil {
			t.Fatalf("New() = %v", err)
		}
		if err := writer.Put(ctx, ns, format, key, Entry{
			Meta: EntryMeta{ContentType: "application/json", Status: 200},
			Body: body,
		}); err != nil {
			t.Fatalf("Put() = %v", err)
		}

		reader, err := New(b, "")
		if err != nil {
			t.Fatalf("New() = %v", err)
		}
		got, found, err := reader.Get(ctx, ns, format, key)
		if err != nil || !found {
			t.Fatalf("Get() = (_, %v, %v), want found", found, err)
		}
		if string(got.Body) != string(body) {
			t.Fatalf("body = %q, want %q", got.Body, body)
		}
		if got.Meta.Key != key {
			t.Fatalf("meta.Key = %q, want %q", got.Meta.Key, key)
		}
	})
}

// TestInvisibleToStoreListings proves cache objects never appear in a scoped
// core.Store's package/version/file listings — they live under .cache/, a
// dot-entry dropped at every level.
func TestInvisibleToStoreListings(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		const ns, format = "team-a", "pypi"

		c, err := New(b, "")
		if err != nil {
			t.Fatalf("cache New() = %v", err)
		}
		if err := c.Put(ctx, ns, format, "pypi:simple:requests", Entry{Body: []byte("cached")}); err != nil {
			t.Fatalf("Put() = %v", err)
		}

		store, err := blobstore.NewWithBucket(b, ns+"/"+format)
		if err != nil {
			t.Fatalf("blobstore.NewWithBucket() = %v", err)
		}

		pkgs, err := store.Packages(ctx)
		if err != nil {
			t.Fatalf("Packages() = %v", err)
		}
		if len(pkgs) != 0 {
			names := make([]string, len(pkgs))
			for i, p := range pkgs {
				names[i] = p.Name()
			}
			t.Fatalf("Packages() = %v, want empty (cache entries must be invisible)", names)
		}

		// A real package added alongside the cache is the only thing listed.
		if _, err := store.AddPackage(ctx, "flask"); err != nil {
			t.Fatalf("AddPackage() = %v", err)
		}
		pkgs, err = store.Packages(ctx)
		if err != nil {
			t.Fatalf("Packages() = %v", err)
		}
		if len(pkgs) != 1 || pkgs[0].Name() != "flask" {
			t.Fatalf("Packages() = %v, want [flask] only", pkgs)
		}
	})
}
