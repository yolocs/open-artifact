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

// TestCacheRoundTrip exercises the cache as a File: AddCache streams a blob,
// Cache(key) reads it back (digest-verified) with metadata, and Delete removes
// it.
func TestCacheRoundTrip(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		s, err := NewWithBucket(b, testScope)
		if err != nil {
			t.Fatalf("NewWithBucket: %v", err)
		}
		pkg := s.Package("requests")
		const key = "simple"
		body := []byte("<html>requests</html>")

		cf, err := pkg.AddCache(ctx, key, bytes.NewReader(body))
		if err != nil {
			t.Fatalf("AddCache = %v", err)
		}
		if cf.Name() != key {
			t.Fatalf("Name = %q, want %q", cf.Name(), key)
		}

		got := pkg.Cache(key)
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

		// Cache is mutable: a second AddCache overwrites without ErrAlreadyExists.
		if _, err := pkg.AddCache(ctx, key, bytes.NewReader([]byte("v2"))); err != nil {
			t.Fatalf("AddCache overwrite = %v", err)
		}
		rc2, err := pkg.Cache(key).Read(ctx)
		if err != nil {
			t.Fatalf("Read after overwrite = %v", err)
		}
		if data := readAll(t, rc2); data != "v2" {
			t.Fatalf("after overwrite Read = %q, want v2", data)
		}

		if err := pkg.Cache(key).Delete(ctx); err != nil {
			t.Fatalf("Delete = %v", err)
		}
		if ok, err := pkg.Cache(key).Exists(ctx); err != nil || ok {
			t.Fatalf("Exists after Delete = (%v, %v), want false", ok, err)
		}
		// Deleting an absent entry is not an error.
		if err := pkg.Cache(key).Delete(ctx); err != nil {
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
		if _, err := s.AddCache(ctx, key, bytes.NewReader([]byte("store"))); err != nil {
			t.Fatalf("store AddCache = %v", err)
		}
		if _, err := pkg.AddCache(ctx, key, bytes.NewReader([]byte("package"))); err != nil {
			t.Fatalf("package AddCache = %v", err)
		}
		if _, err := ver.AddCache(ctx, key, bytes.NewReader([]byte("version"))); err != nil {
			t.Fatalf("version AddCache = %v", err)
		}

		check := func(name string, cf core.CacheFile, want string) {
			rc, err := cf.Read(ctx)
			if err != nil {
				t.Fatalf("%s Read = %v", name, err)
			}
			if got := readAll(t, rc); got != want {
				t.Fatalf("%s cache = %q, want %q", name, got, want)
			}
		}
		check("store", s.Cache(key), "store")
		check("package", pkg.Cache(key), "package")
		check("version", ver.Cache(key), "version")
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

		if _, err := s.AddCache(ctx, "root-index", bytes.NewReader([]byte("x"))); err != nil {
			t.Fatalf("store AddCache = %v", err)
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
		if _, err := pkg.AddCache(ctx, "simple", bytes.NewReader([]byte("x"))); err != nil {
			t.Fatalf("package AddCache = %v", err)
		}
		if vers, err := pkg.Versions(ctx); err != nil || len(vers) != 0 {
			t.Fatalf("Versions = (%v, %v), want empty (cache blob is not a version)", vers, err)
		}

		ver := pkg.Version("2.31.0")
		if _, err := ver.AddCache(ctx, "meta", bytes.NewReader([]byte("x"))); err != nil {
			t.Fatalf("version AddCache = %v", err)
		}
		if files, err := ver.Files(ctx); err != nil || len(files) != 0 {
			t.Fatalf("Files = (%v, %v), want empty (cache blob is not a file)", files, err)
		}
	})
}

// TestCacheAuthorizesAsRead proves filling the cache needs only reader policy:
// a guard that allows reads but denies writes still permits AddCache, while a
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
		if _, err := s.AddCache(ctx, "k", bytes.NewReader([]byte("x"))); err != nil {
			t.Fatalf("AddCache under read-only guard = %v, want allowed (reader fills cache)", err)
		}
		if _, err := s.Cache("k").Read(ctx); err != nil {
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
		if _, err := s2.AddCache(ctx, "k", bytes.NewReader([]byte("x"))); !errors.Is(err, errDenied) {
			t.Fatalf("AddCache under read-denying guard = %v, want errDenied", err)
		}
	})
}
