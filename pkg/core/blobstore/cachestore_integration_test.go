//go:build integration

package blobstore

import (
	"errors"
	"testing"

	"gocloud.dev/blob"
)

func TestCacheRoundTrip(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		s, err := NewWithBucket(b, testScope)
		if err != nil {
			t.Fatalf("NewWithBucket: %v", err)
		}

		c := s.Cache()
		body := []byte("<html>requests</html>")
		if err := c.Put(ctx, "simple:requests", body); err != nil {
			t.Fatalf("Put = %v", err)
		}
		got, found, err := c.Get(ctx, "simple:requests")
		if err != nil || !found {
			t.Fatalf("Get = (_, %v, %v), want found", found, err)
		}
		if string(got.Body) != string(body) {
			t.Fatalf("body = %q, want %q", got.Body, body)
		}
		if got.ModTime.IsZero() {
			t.Fatalf("ModTime is zero; surfaces need it for freshness")
		}

		// Overwrite, then delete.
		if err := c.Put(ctx, "simple:requests", []byte("v2")); err != nil {
			t.Fatalf("Put overwrite = %v", err)
		}
		if got, _, _ := c.Get(ctx, "simple:requests"); string(got.Body) != "v2" {
			t.Fatalf("after overwrite body = %q, want v2", got.Body)
		}
		if err := c.Delete(ctx, "simple:requests"); err != nil {
			t.Fatalf("Delete = %v", err)
		}
		if _, found, _ := c.Get(ctx, "simple:requests"); found {
			t.Fatalf("Get after Delete found = true")
		}
		// Deleting an absent entry is not an error.
		if err := c.Delete(ctx, "simple:requests"); err != nil {
			t.Fatalf("Delete absent = %v, want nil", err)
		}
	})
}

// TestCacheLevelsIsolated proves the store/package/version caches are
// independent: the same key at each level addresses a distinct blob.
func TestCacheLevelsIsolated(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		s, err := NewWithBucket(b, testScope)
		if err != nil {
			t.Fatalf("NewWithBucket: %v", err)
		}
		pkg := s.Package("requests")
		ver := pkg.Version("2.31.0")

		const key = "index"
		if err := s.Cache().Put(ctx, key, []byte("store")); err != nil {
			t.Fatalf("store Put = %v", err)
		}
		if err := pkg.Cache().Put(ctx, key, []byte("package")); err != nil {
			t.Fatalf("package Put = %v", err)
		}
		if err := ver.Cache().Put(ctx, key, []byte("version")); err != nil {
			t.Fatalf("version Put = %v", err)
		}

		assert := func(name string, got []byte, want string) {
			if string(got) != want {
				t.Fatalf("%s cache = %q, want %q", name, got, want)
			}
		}
		sg, _, _ := s.Cache().Get(ctx, key)
		pg, _, _ := pkg.Cache().Get(ctx, key)
		vg, _, _ := ver.Cache().Get(ctx, key)
		assert("store", sg.Body, "store")
		assert("package", pg.Body, "package")
		assert("version", vg.Body, "version")
	})
}

// TestFormatCacheInvisibleToPackages proves the format-level cache (the proxy
// pull-through level) never shows up as a package: its .cache/ child is dropped,
// and a real package added beside it is the only thing listed.
func TestFormatCacheInvisibleToPackages(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		s, err := NewWithBucket(b, testScope)
		if err != nil {
			t.Fatalf("NewWithBucket: %v", err)
		}

		if err := s.Cache().Put(ctx, "root-index", []byte("x")); err != nil {
			t.Fatalf("store Put = %v", err)
		}
		if pkgs, err := s.Packages(ctx); err != nil || len(pkgs) != 0 {
			t.Fatalf("Packages = (%v, %v), want empty (format cache must be invisible)", pkgs, err)
		}

		if _, err := s.AddPackage(ctx, "flask"); err != nil {
			t.Fatalf("AddPackage = %v", err)
		}
		pkgs, err := s.Packages(ctx)
		if err != nil {
			t.Fatalf("Packages = %v", err)
		}
		if len(pkgs) != 1 || pkgs[0].Name() != "flask" {
			t.Fatalf("Packages = %v, want [flask] only", pkgs)
		}
	})
}

// TestCacheBlobNotListedAtItsLevel proves a cache blob is never returned as a
// child entry of its owning level — a package's cache blob is not a Version, a
// version's cache blob is not a File. (Writing a package/version-level cache
// does, like any object, materialize that package/version directory, so it
// becomes observable in the parent listing; that is expected.)
func TestCacheBlobNotListedAtItsLevel(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		s, err := NewWithBucket(b, testScope)
		if err != nil {
			t.Fatalf("NewWithBucket: %v", err)
		}

		pkg := s.Package("requests")
		if err := pkg.Cache().Put(ctx, "simple", []byte("x")); err != nil {
			t.Fatalf("package Put = %v", err)
		}
		if vers, err := pkg.Versions(ctx); err != nil || len(vers) != 0 {
			t.Fatalf("Versions = (%v, %v), want empty (cache blob is not a version)", vers, err)
		}

		ver := pkg.Version("2.31.0")
		if err := ver.Cache().Put(ctx, "meta", []byte("x")); err != nil {
			t.Fatalf("version Put = %v", err)
		}
		if files, err := ver.Files(ctx); err != nil || len(files) != 0 {
			t.Fatalf("Files = (%v, %v), want empty (cache blob is not a file)", files, err)
		}
	})
}

// TestCacheAuthorizesAsRead proves filling the cache needs only reader policy:
// a guard that allows reads but denies writes still permits Cache.Put, while a
// guard that denies reads blocks it.
func TestCacheAuthorizesAsRead(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()

		// Reader allowed, writer denied: cache fill must still succeed.
		readOnly := &recordingGuard{denyWrite: true}
		s, err := NewWithBucket(b, testScope, WithGuard(readOnly.guard))
		if err != nil {
			t.Fatalf("NewWithBucket: %v", err)
		}
		if err := s.Cache().Put(ctx, "k", []byte("x")); err != nil {
			t.Fatalf("Cache.Put under read-only guard = %v, want allowed (reader fills cache)", err)
		}
		if _, _, err := s.Cache().Get(ctx, "k"); err != nil {
			t.Fatalf("Cache.Get under read-only guard = %v, want allowed", err)
		}
		if readOnly.writes != 0 {
			t.Fatalf("cache ops consulted the guard as writes (%d); want reads only", readOnly.writes)
		}

		// Reader denied: cache fill must be rejected.
		noRead := &recordingGuard{denyRead: true}
		s2, err := NewWithBucket(b, testScope, WithGuard(noRead.guard))
		if err != nil {
			t.Fatalf("NewWithBucket: %v", err)
		}
		if err := s2.Cache().Put(ctx, "k", []byte("x")); !errors.Is(err, errDenied) {
			t.Fatalf("Cache.Put under read-denying guard = %v, want errDenied", err)
		}
	})
}
