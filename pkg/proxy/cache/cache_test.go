package cache

import (
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"gocloud.dev/blob/memblob"

	"github.com/yolocs/open-artifact/pkg/core"
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
	got := hashKey("npm:packument:@scope/name")
	for _, r := range got {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			t.Fatalf("hashKey produced non-hex char %q in %q", r, got)
		}
	}
}

func newCache(t *testing.T, now func() time.Time) *Cache {
	t.Helper()
	b := memblob.OpenBucket(nil)
	t.Cleanup(func() { b.Close() })
	opts := []Option{}
	if now != nil {
		opts = append(opts, withClock(now))
	}
	c, err := New(b, "", opts...)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	return c
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()

	c := newCache(t, nil)
	ctx := t.Context()
	const ns, format, key = "team-a", "pypi", "pypi:simple:requests"
	body := []byte("<html>requests</html>")

	in := Entry{
		Meta: EntryMeta{
			ContentType:  "text/html",
			Status:       200,
			FetchedAt:    time.Date(2026, 5, 22, 1, 2, 3, 0, time.UTC),
			ETag:         `"etag-1"`,
			LastModified: "Wed, 21 Oct 2026 07:28:00 GMT",
		},
		Body: body,
	}
	if err := c.Put(ctx, ns, format, key, in); err != nil {
		t.Fatalf("Put() = %v", err)
	}

	got, found, err := c.Get(ctx, ns, format, key)
	if err != nil || !found {
		t.Fatalf("Get() = (_, %v, %v), want found", found, err)
	}
	if string(got.Body) != string(body) {
		t.Fatalf("body = %q, want %q", got.Body, body)
	}
	wantMeta := EntryMeta{
		Key:          key,
		ContentType:  "text/html",
		Status:       200,
		FetchedAt:    in.Meta.FetchedAt,
		ETag:         `"etag-1"`,
		LastModified: "Wed, 21 Oct 2026 07:28:00 GMT",
		Digest:       digestOf(body),
	}
	if diff := cmp.Diff(wantMeta, got.Meta); diff != "" {
		t.Fatalf("meta mismatch (-want +got):\n%s", diff)
	}
	if got.Stale {
		t.Fatalf("fresh entry reported Stale")
	}
}

func TestGetMissing(t *testing.T) {
	t.Parallel()

	c := newCache(t, nil)
	_, found, err := c.Get(t.Context(), "ns", "pypi", "absent")
	if err != nil {
		t.Fatalf("Get() = %v", err)
	}
	if found {
		t.Fatalf("Get() found = true for absent key")
	}
}

func TestFreshForeverWhenNoExpiry(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := newCache(t, func() time.Time { return now })
	ctx := t.Context()

	if err := c.Put(ctx, "ns", "pypi", "k", Entry{Body: []byte("x")}); err != nil {
		t.Fatalf("Put() = %v", err)
	}
	now = now.Add(1000 * time.Hour)
	if _, found, err := c.Get(ctx, "ns", "pypi", "k"); err != nil || !found {
		t.Fatalf("Get() = (_, %v, %v), want fresh-forever entry found", found, err)
	}
}

func TestStaleVsFresh(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := newCache(t, func() time.Time { return now })
	ctx := t.Context()

	exp := now.Add(10 * time.Minute)
	if err := c.Put(ctx, "ns", "pypi", "k", Entry{Meta: EntryMeta{ExpiresAt: exp}, Body: []byte("x")}); err != nil {
		t.Fatalf("Put() = %v", err)
	}

	// Before expiry: fresh.
	if _, found, err := c.Get(ctx, "ns", "pypi", "k"); err != nil || !found {
		t.Fatalf("Get() before expiry = (_, %v, %v), want found", found, err)
	}

	// After expiry: Get reports a miss, GetStale serves it flagged stale.
	now = exp.Add(time.Second)
	if _, found, err := c.Get(ctx, "ns", "pypi", "k"); err != nil || found {
		t.Fatalf("Get() after expiry = (_, %v, %v), want not found", found, err)
	}
	got, found, err := c.GetStale(ctx, "ns", "pypi", "k")
	if err != nil || !found {
		t.Fatalf("GetStale() = (_, %v, %v), want found", found, err)
	}
	if !got.Stale {
		t.Fatalf("GetStale() Stale = false for expired entry")
	}
	if string(got.Body) != "x" {
		t.Fatalf("GetStale() body = %q", got.Body)
	}
}

func TestGetStaleOnFreshEntry(t *testing.T) {
	t.Parallel()

	c := newCache(t, nil)
	ctx := t.Context()
	if err := c.Put(ctx, "ns", "pypi", "k", Entry{Body: []byte("x")}); err != nil {
		t.Fatalf("Put() = %v", err)
	}
	got, found, err := c.GetStale(ctx, "ns", "pypi", "k")
	if err != nil || !found {
		t.Fatalf("GetStale() = (_, %v, %v), want found", found, err)
	}
	if got.Stale {
		t.Fatalf("GetStale() Stale = true for fresh entry")
	}
}

func TestDigestMismatch(t *testing.T) {
	t.Parallel()

	c := newCache(t, nil)
	ctx := t.Context()
	const ns, format, key = "ns", "pypi", "k"
	if err := c.Put(ctx, ns, format, key, Entry{Body: []byte("correct")}); err != nil {
		t.Fatalf("Put() = %v", err)
	}

	// Corrupt the stored body behind the unchanged metadata digest.
	if err := c.bucket.WriteAll(ctx, c.base(ns, format, key)+bodyExt, []byte("tampered"), nil); err != nil {
		t.Fatalf("corrupt body: %v", err)
	}

	if _, _, err := c.Get(ctx, ns, format, key); !errors.Is(err, core.ErrDigestMismatch) {
		t.Fatalf("Get() = %v, want core.ErrDigestMismatch", err)
	}
}

func TestDelete(t *testing.T) {
	t.Parallel()

	c := newCache(t, nil)
	ctx := t.Context()
	const ns, format, key = "ns", "pypi", "k"

	if err := c.Put(ctx, ns, format, key, Entry{Body: []byte("x")}); err != nil {
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

func TestPrefixInPath(t *testing.T) {
	t.Parallel()

	b := memblob.OpenBucket(nil)
	t.Cleanup(func() { b.Close() })
	c, err := New(b, "deploy1")
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	got := c.base("team-a", "pypi", "k")
	want := "open-artifact/v1/deploy1/team-a/pypi/.cache/" + hashKey("k")
	if got != want {
		t.Fatalf("base() = %q, want %q", got, want)
	}
}
