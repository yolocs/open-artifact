package namespace

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"gocloud.dev/blob"

	"github.com/yolocs/open-artifact/pkg/core/blobstore"
)

func newRegistry(t *testing.T, b *blob.Bucket, prefix string) (*Store, *Registry) {
	t.Helper()
	s, err := NewStore(b, prefix)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	reg, err := NewRegistry(b, prefix, s)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return s, reg
}

func TestRegistryForInvalidInputs(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		_, reg := newRegistry(t, b, "")

		if _, err := reg.For("Bad_Name", "pypi"); !errors.Is(err, ErrInvalidName) {
			t.Errorf("For(bad name) = %v, want ErrInvalidName", err)
		}
		if _, err := reg.For("team-a", "cargo"); err == nil {
			t.Errorf("For(bad format) = nil, want error")
		}
		for _, f := range []string{"pypi", "npm", "maven"} {
			if _, err := reg.For("team-a", f); err != nil {
				t.Errorf("For(team-a, %q) = %v, want nil", f, err)
			}
		}
	})
}

func TestRegistrySpecVisibleWithoutRestart(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		store, reg := newRegistry(t, b, "")

		scoped, err := reg.For("team-a", "pypi")
		if err != nil {
			t.Fatalf("For: %v", err)
		}
		// Unknown namespace maps to ErrNotFound for data-plane callers.
		if _, err := scoped.Spec(ctx); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Spec(unknown) = %v, want ErrNotFound", err)
		}

		// Create via the catalog; the factory sees it without reconstruction.
		if _, err := store.Put(ctx, &Namespace{Name: "team-a", Spec: Spec{Mode: ModeProxy, Proxy: Proxy{Upstream: "https://pypi.org/simple"}}}); err != nil {
			t.Fatalf("Put: %v", err)
		}
		spec, err := scoped.Spec(ctx)
		if err != nil {
			t.Fatalf("Spec: %v", err)
		}
		if !spec.IsProxy() || spec.Proxy.Upstream != "https://pypi.org/simple" {
			t.Errorf("Spec = %+v, want proxy with upstream", spec)
		}
	})
}

func TestRegistryStoreScoping(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		_, reg := newRegistry(t, b, "shared")

		scoped, err := reg.For("team-a", "pypi")
		if err != nil {
			t.Fatalf("For: %v", err)
		}
		ds, err := scoped.Store()
		if err != nil {
			t.Fatalf("Store: %v", err)
		}
		if _, err := ds.Package("requests").Version("2.31.0").AddFile(ctx, "requests-2.31.0.whl", bytes.NewReader([]byte("data"))); err != nil {
			t.Fatalf("AddFile: %v", err)
		}

		// Bytes land under <root>/<prefix>/<ns>/<fmt>/<package>/<version>/<file>.
		want := blobstore.Root + "shared/team-a/pypi/requests/2.31.0/requests-2.31.0.whl"
		if ok, _ := b.Exists(ctx, want); !ok {
			t.Errorf("expected blob at %q", want)
		}
	})
}

func TestRegistryNPMScopedName(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		store, reg := newRegistry(t, b, "")
		if _, err := store.Put(ctx, &Namespace{Name: "team-a"}); err != nil {
			t.Fatalf("Put: %v", err)
		}

		scoped, err := reg.For("team-a", "npm")
		if err != nil {
			t.Fatalf("For: %v", err)
		}
		ds, err := scoped.Store()
		if err != nil {
			t.Fatalf("Store: %v", err)
		}

		const scopedName = "@scope/name"
		body := []byte("tarball")
		if _, err := ds.Package(scopedName).Version("1.0.0").AddFile(ctx, "name-1.0.0.tgz", bytes.NewReader(body)); err != nil {
			t.Fatalf("AddFile: %v", err)
		}

		// The "/" in the scoped name is encoded into a single path segment, so
		// the package round-trips losslessly through listing.
		pkgs, err := ds.Packages(ctx)
		if err != nil {
			t.Fatalf("Packages: %v", err)
		}
		if len(pkgs) != 1 || pkgs[0].Name() != scopedName {
			var got []string
			for _, p := range pkgs {
				got = append(got, p.Name())
			}
			t.Fatalf("Packages = %v, want [%q]", got, scopedName)
		}

		// And the bytes read back through the chained handle.
		rc, err := ds.Package(scopedName).Version("1.0.0").File("name-1.0.0.tgz").Read(ctx)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		defer rc.Close()
		got, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if !bytes.Equal(got, body) {
			t.Errorf("read = %q, want %q", got, body)
		}

		// The scoped namespace counts as non-empty for delete.
		if err := store.Delete(ctx, "team-a"); !errors.Is(err, ErrNotEmpty) {
			t.Errorf("Delete(scoped-data) = %v, want ErrNotEmpty", err)
		}
	})
}
