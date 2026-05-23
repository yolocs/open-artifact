//go:build integration

package blobstore

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"gocloud.dev/blob"

	"github.com/yolocs/open-artifact/pkg/core"
)

func readAll(t *testing.T, rc io.ReadCloser) string {
	t.Helper()
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(b)
}

// TestCacheRoundTrip exercises the cache as a File: Put streams a blob, File
// reads it back (digest-verified) with metadata, and Delete removes it.
func TestCacheRoundTrip(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		s, err := NewWithBucket(b, testScope)
		if err != nil {
			t.Fatalf("NewWithBucket: %v", err)
		}
		c := s.Package("requests").Cache()
		const key = "simple"
		body := []byte("<html>requests</html>")

		cf, err := c.Put(ctx, key, bytes.NewReader(body))
		if err != nil {
			t.Fatalf("Put = %v", err)
		}
		if cf.Name() != key {
			t.Fatalf("Name = %q, want %q", cf.Name(), key)
		}

		got := c.File(key)
		if ok, err := got.Exists(ctx); err != nil || !ok {
			t.Fatalf("Exists = (%v, %v), want true", ok, err)
		}
		rc, err := got.Read(ctx)
		if err != nil {
			t.Fatalf("Read = %v", err)
		}
		if data := readAll(t, rc); data != string(body) {
			t.Fatalf("Read = %q, want %q", data, body)
		}
		meta, err := got.Meta(ctx)
		if err != nil {
			t.Fatalf("Meta = %v", err)
		}
		if meta.Digest == "" || meta.UpdatedAt.IsZero() {
			t.Fatalf("Meta missing digest/timestamp: %+v", meta)
		}

		// Cache is mutable: a second Put overwrites without ErrAlreadyExists.
		if _, err := c.Put(ctx, key, bytes.NewReader([]byte("v2"))); err != nil {
			t.Fatalf("Put overwrite = %v", err)
		}
		rc2, err := c.File(key).Read(ctx)
		if err != nil {
			t.Fatalf("Read after overwrite = %v", err)
		}
		if data := readAll(t, rc2); data != "v2" {
			t.Fatalf("after overwrite Read = %q, want v2", data)
		}

		if err := c.Delete(ctx, key); err != nil {
			t.Fatalf("Delete = %v", err)
		}
		if ok, err := c.File(key).Exists(ctx); err != nil || ok {
			t.Fatalf("Exists after Delete = (%v, %v), want false", ok, err)
		}
		// Deleting an absent entry is not an error.
		if err := c.Delete(ctx, key); err != nil {
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
		put := func(c core.Cache, body string) {
			if _, err := c.Put(ctx, key, bytes.NewReader([]byte(body))); err != nil {
				t.Fatalf("Put = %v", err)
			}
		}
		put(s.Cache(), "store")
		put(pkg.Cache(), "package")
		put(ver.Cache(), "version")

		check := func(name string, c core.Cache, want string) {
			rc, err := c.File(key).Read(ctx)
			if err != nil {
				t.Fatalf("%s Read = %v", name, err)
			}
			if got := readAll(t, rc); got != want {
				t.Fatalf("%s cache = %q, want %q", name, got, want)
			}
		}
		check("store", s.Cache(), "store")
		check("package", pkg.Cache(), "package")
		check("version", ver.Cache(), "version")
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

		if _, err := s.Cache().Put(ctx, "root-index", bytes.NewReader([]byte("x"))); err != nil {
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
		if _, err := pkg.Cache().Put(ctx, "simple", bytes.NewReader([]byte("x"))); err != nil {
			t.Fatalf("package Put = %v", err)
		}
		if vers, err := pkg.Versions(ctx); err != nil || len(vers) != 0 {
			t.Fatalf("Versions = (%v, %v), want empty (cache blob is not a version)", vers, err)
		}

		ver := pkg.Version("2.31.0")
		if _, err := ver.Cache().Put(ctx, "meta", bytes.NewReader([]byte("x"))); err != nil {
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

		readOnly := &recordingGuard{denyWrite: true}
		s, err := NewWithBucket(b, testScope, WithGuard(readOnly.guard))
		if err != nil {
			t.Fatalf("NewWithBucket: %v", err)
		}
		if _, err := s.Cache().Put(ctx, "k", bytes.NewReader([]byte("x"))); err != nil {
			t.Fatalf("Cache.Put under read-only guard = %v, want allowed (reader fills cache)", err)
		}
		if _, err := s.Cache().File("k").Read(ctx); err != nil {
			t.Fatalf("Cache read under read-only guard = %v, want allowed", err)
		}
		if readOnly.writes != 0 {
			t.Fatalf("cache ops consulted the guard as writes (%d); want reads only", readOnly.writes)
		}

		noRead := &recordingGuard{denyRead: true}
		s2, err := NewWithBucket(b, testScope, WithGuard(noRead.guard))
		if err != nil {
			t.Fatalf("NewWithBucket: %v", err)
		}
		if _, err := s2.Cache().Put(ctx, "k", bytes.NewReader([]byte("x"))); !errors.Is(err, errDenied) {
			t.Fatalf("Cache.Put under read-denying guard = %v, want errDenied", err)
		}
	})
}
