package cache

import (
	"testing"

	"gocloud.dev/blob/memblob"
)

func TestHashKeyStableAndDistinct(t *testing.T) {
	t.Parallel()

	if a, b := hashKey("pypi:simple:requests"), hashKey("pypi:simple:requests"); a != b {
		t.Fatalf("hashKey not stable: %q != %q", a, b)
	}
	if a, b := hashKey("pypi:simple:requests"), hashKey("npm:packument:@scope/name"); a == b {
		t.Fatalf("distinct keys hashed identically: %q", a)
	}
	// A key with slashes must produce a single path-safe segment (hex only).
	for _, r := range hashKey("npm:packument:@scope/name") {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			t.Fatalf("hashKey produced non-hex char %q", r)
		}
	}
}

func newCache(t *testing.T) *Cache {
	t.Helper()
	b := memblob.OpenBucket(nil)
	t.Cleanup(func() { b.Close() })
	c, err := New(b, "")
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	return c
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()

	c := newCache(t)
	ctx := t.Context()
	const ns, format, key = "team-a", "pypi", "simple:requests"
	body := []byte("<html>requests</html>")

	if err := c.Put(ctx, ns, format, key, body); err != nil {
		t.Fatalf("Put() = %v", err)
	}
	got, found, err := c.Get(ctx, ns, format, key)
	if err != nil || !found {
		t.Fatalf("Get() = (_, %v, %v), want found", found, err)
	}
	if string(got.Body) != string(body) {
		t.Fatalf("body = %q, want %q", got.Body, body)
	}
	if got.ModTime.IsZero() {
		t.Fatalf("Get() ModTime is zero; surfaces need it for freshness")
	}
}

func TestGetMissing(t *testing.T) {
	t.Parallel()

	_, found, err := newCache(t).Get(t.Context(), "ns", "pypi", "absent")
	if err != nil {
		t.Fatalf("Get() = %v", err)
	}
	if found {
		t.Fatalf("Get() found = true for absent key")
	}
}

func TestPutOverwrites(t *testing.T) {
	t.Parallel()

	c := newCache(t)
	ctx := t.Context()
	const ns, format, key = "ns", "npm", "packument:left-pad"

	if err := c.Put(ctx, ns, format, key, []byte("v1")); err != nil {
		t.Fatalf("Put() = %v", err)
	}
	if err := c.Put(ctx, ns, format, key, []byte("v2")); err != nil {
		t.Fatalf("Put() overwrite = %v", err)
	}
	got, found, err := c.Get(ctx, ns, format, key)
	if err != nil || !found {
		t.Fatalf("Get() = (_, %v, %v), want found", found, err)
	}
	if string(got.Body) != "v2" {
		t.Fatalf("body = %q, want %q", got.Body, "v2")
	}
}

func TestKeyIsolation(t *testing.T) {
	t.Parallel()

	c := newCache(t)
	ctx := t.Context()
	if err := c.Put(ctx, "team-a", "pypi", "simple:requests", []byte("a")); err != nil {
		t.Fatalf("Put() = %v", err)
	}
	// Different namespace, format, or key must not collide.
	for _, miss := range []struct{ ns, fmt, key string }{
		{"team-b", "pypi", "simple:requests"},
		{"team-a", "npm", "simple:requests"},
		{"team-a", "pypi", "simple:flask"},
	} {
		if _, found, err := c.Get(ctx, miss.ns, miss.fmt, miss.key); err != nil || found {
			t.Fatalf("Get(%v) = (_, %v, %v), want not found", miss, found, err)
		}
	}
}

func TestDelete(t *testing.T) {
	t.Parallel()

	c := newCache(t)
	ctx := t.Context()
	const ns, format, key = "ns", "pypi", "simple:requests"

	if err := c.Put(ctx, ns, format, key, []byte("x")); err != nil {
		t.Fatalf("Put() = %v", err)
	}
	if err := c.Delete(ctx, ns, format, key); err != nil {
		t.Fatalf("Delete() = %v", err)
	}
	if _, found, err := c.Get(ctx, ns, format, key); err != nil || found {
		t.Fatalf("Get() after Delete = (_, %v, %v), want not found", found, err)
	}
	// Deleting an already-absent entry is not an error.
	if err := c.Delete(ctx, ns, format, key); err != nil {
		t.Fatalf("Delete() on absent = %v, want nil", err)
	}
}

func TestPathLayout(t *testing.T) {
	t.Parallel()

	b := memblob.OpenBucket(nil)
	t.Cleanup(func() { b.Close() })

	plain, _ := New(b, "")
	if got, want := plain.path("team-a", "pypi", "k"), "open-artifact/v1/team-a/pypi/.cache/"+hashKey("k"); got != want {
		t.Fatalf("path() = %q, want %q", got, want)
	}
	prefixed, _ := New(b, "deploy1")
	if got, want := prefixed.path("team-a", "pypi", "k"), "open-artifact/v1/deploy1/team-a/pypi/.cache/"+hashKey("k"); got != want {
		t.Fatalf("path() = %q, want %q", got, want)
	}
}
