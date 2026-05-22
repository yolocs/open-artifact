//go:build integration

package blobstore

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"gocloud.dev/blob"
)

var errDenied = errors.New("denied")

// recordingGuard denies operations whose write flag is in its deny set and
// records every call it sees.
type recordingGuard struct {
	denyRead  bool
	denyWrite bool
	reads     int
	writes    int
}

func (g *recordingGuard) guard(_ context.Context, write bool) error {
	if write {
		g.writes++
		if g.denyWrite {
			return errDenied
		}
		return nil
	}
	g.reads++
	if g.denyRead {
		return errDenied
	}
	return nil
}

func TestGuardAllowsWhenPermitted(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		g := &recordingGuard{}
		s, err := NewWithBucket(b, testScope, WithGuard(g.guard))
		if err != nil {
			t.Fatalf("NewWithBucket: %v", err)
		}

		if _, err := s.Package("p").Version("1.0.0").AddFile(ctx, "p-1.0.0.whl", bytes.NewReader([]byte("x"))); err != nil {
			t.Fatalf("AddFile = %v, want allowed", err)
		}
		if _, err := s.Packages(ctx); err != nil {
			t.Fatalf("Packages = %v, want allowed", err)
		}
		if g.writes == 0 || g.reads == 0 {
			t.Errorf("guard not consulted: reads=%d writes=%d", g.reads, g.writes)
		}
	})
}

func TestGuardDeniesWrite(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		g := &recordingGuard{denyWrite: true}
		s, _ := NewWithBucket(b, testScope, WithGuard(g.guard))

		_, err := s.AddPackage(ctx, "requests")
		if !errors.Is(err, errDenied) {
			t.Fatalf("AddPackage = %v, want errDenied", err)
		}
		// The denial happens before any bucket write: nothing was created.
		ok, err := b.Exists(ctx, packageMetaPath(testScope, "requests"))
		if err != nil {
			t.Fatalf("Exists: %v", err)
		}
		if ok {
			t.Error("denied write still created the .meta object")
		}
	})
}

func TestGuardDeniesRead(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		// Seed data with an unguarded store, then read through a guarded one.
		seed, _ := NewWithBucket(b, testScope)
		if _, err := seed.Package("p").Version("1.0.0").AddFile(ctx, "f.whl", bytes.NewReader([]byte("x"))); err != nil {
			t.Fatalf("seed AddFile: %v", err)
		}

		g := &recordingGuard{denyRead: true}
		s, _ := NewWithBucket(b, testScope, WithGuard(g.guard))

		if _, err := s.Packages(ctx); !errors.Is(err, errDenied) {
			t.Errorf("Packages = %v, want errDenied", err)
		}
		if _, err := s.Package("p").Version("1.0.0").File("f.whl").Read(ctx); !errors.Is(err, errDenied) {
			t.Errorf("Read = %v, want errDenied", err)
		}
		if _, err := s.Package("p").Exists(ctx); !errors.Is(err, errDenied) {
			t.Errorf("Exists = %v, want errDenied", err)
		}
	})
}

func TestNilGuardIsTransparent(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		s, _ := NewWithBucket(b, testScope) // no guard
		if _, err := s.AddPackage(ctx, "requests"); err != nil {
			t.Fatalf("AddPackage without guard = %v, want allowed", err)
		}
	})
}
