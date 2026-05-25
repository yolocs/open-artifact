package blobstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"gocloud.dev/blob"
	"gocloud.dev/blob/memblob"

	"github.com/yolocs/open-artifact/pkg/core"
)

func packageNames(ps []core.Package) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Name())
	}
	return out
}

func versionNames(vs []core.Version) []string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		out = append(out, v.Name())
	}
	return out
}

func fileNames(fs []core.File) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Name())
	}
	return out
}

func tagNames(ts []core.Tag) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name())
	}
	return out
}

func TestListingsReturnRealChildrenOnly(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		s, _ := NewWithBucket(b, testScope)

		// alpha: two versions (one with two files), plus a .meta and .tags.
		if _, err := s.AddPackage(ctx, "alpha"); err != nil {
			t.Fatalf("AddPackage alpha: %v", err)
		}
		v1 := s.Package("alpha").Version("1.0.0")
		if _, err := v1.AddFile(ctx, "a.whl", bytes.NewReader([]byte("a"))); err != nil {
			t.Fatalf("AddFile a.whl: %v", err)
		}
		if _, err := v1.AddFile(ctx, "b.whl", bytes.NewReader([]byte("b"))); err != nil {
			t.Fatalf("AddFile b.whl: %v", err)
		}
		if _, err := s.Package("alpha").Version("2.0.0").AddFile(ctx, "c.whl", bytes.NewReader([]byte("c"))); err != nil {
			t.Fatalf("AddFile c.whl: %v", err)
		}
		if err := s.Package("alpha").SetTag(ctx, "latest", "2.0.0"); err != nil {
			t.Fatalf("SetTag: %v", err)
		}
		// beta: package envelope only.
		if _, err := s.AddPackage(ctx, "beta"); err != nil {
			t.Fatalf("AddPackage beta: %v", err)
		}

		pkgs, err := s.Packages(ctx)
		if err != nil {
			t.Fatalf("Packages: %v", err)
		}
		if diff := cmp.Diff([]string{"alpha", "beta"}, packageNames(pkgs)); diff != "" {
			t.Errorf("Packages (-want +got):\n%s", diff)
		}

		vers, err := s.Package("alpha").Versions(ctx)
		if err != nil {
			t.Fatalf("Versions: %v", err)
		}
		if diff := cmp.Diff([]string{"1.0.0", "2.0.0"}, versionNames(vers)); diff != "" {
			t.Errorf("Versions (-want +got):\n%s", diff)
		}

		files, err := v1.Files(ctx)
		if err != nil {
			t.Fatalf("Files: %v", err)
		}
		// .meta and .meta.<file> sidecars must be filtered out.
		if diff := cmp.Diff([]string{"a.whl", "b.whl"}, fileNames(files)); diff != "" {
			t.Errorf("Files (-want +got):\n%s", diff)
		}
	})
}

func TestListEmptyScopeReturnsEmptySlice(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		s, _ := NewWithBucket(b, "empty/scope")

		pkgs, err := s.Packages(ctx)
		if err != nil {
			t.Fatalf("Packages: %v", err)
		}
		if pkgs == nil {
			t.Fatal("Packages returned nil, want empty non-nil slice")
		}
		if len(pkgs) != 0 {
			t.Errorf("Packages len = %d, want 0", len(pkgs))
		}

		// Listing under a never-written package likewise yields an empty slice.
		vers, err := s.Package("ghost").Versions(ctx)
		if err != nil {
			t.Fatalf("Versions: %v", err)
		}
		if len(vers) != 0 {
			t.Errorf("Versions len = %d, want 0", len(vers))
		}
	})
}

func TestResolveTagHitAndMiss(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		s, _ := NewWithBucket(b, testScope)
		p := s.Package("requests")

		// Miss: no .tags object yet.
		if _, err := p.Tag("latest").Ref(ctx); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("Ref on missing tag: got %v, want ErrNotFound", err)
		}
		if ok, err := p.Tag("latest").Exists(ctx); err != nil || ok {
			t.Fatalf("Exists on missing tag: got (%v, %v), want (false, nil)", ok, err)
		}

		if err := p.SetTag(ctx, "latest", "2.31.0"); err != nil {
			t.Fatalf("SetTag: %v", err)
		}

		// Hit.
		v, err := p.Tag("latest").Ref(ctx)
		if err != nil {
			t.Fatalf("Ref: %v", err)
		}
		if v.Name() != "2.31.0" {
			t.Errorf("Ref = %q, want %q", v.Name(), "2.31.0")
		}
		if ok, err := p.Tag("latest").Exists(ctx); err != nil || !ok {
			t.Fatalf("Exists on present tag: got (%v, %v), want (true, nil)", ok, err)
		}
		// A different, unset tag still misses.
		if _, err := p.Tag("beta").Ref(ctx); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("Ref beta: got %v, want ErrNotFound", err)
		}
	})
}

func TestListTagsReturnsFullMap(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		s, _ := NewWithBucket(b, testScope)
		p := s.Package("requests")

		// No .tags yet -> empty slice, not error.
		tags, err := p.Tags(ctx)
		if err != nil {
			t.Fatalf("Tags (empty): %v", err)
		}
		if len(tags) != 0 {
			t.Errorf("Tags len = %d, want 0", len(tags))
		}

		want := map[string]string{"latest": "2.31.0", "beta": "3.0.0b1", "lts": "2.0.0"}
		for name, target := range want {
			if err := p.SetTag(ctx, name, target); err != nil {
				t.Fatalf("SetTag %q: %v", name, err)
			}
		}

		tags, err = p.Tags(ctx)
		if err != nil {
			t.Fatalf("Tags: %v", err)
		}
		if diff := cmp.Diff([]string{"beta", "latest", "lts"}, tagNames(tags)); diff != "" {
			t.Errorf("Tags names (-want +got):\n%s", diff)
		}
		// Each tag resolves to its declared target.
		for _, tg := range tags {
			v, err := tg.Ref(ctx)
			if err != nil {
				t.Fatalf("Ref %q: %v", tg.Name(), err)
			}
			if v.Name() != want[tg.Name()] {
				t.Errorf("tag %q -> %q, want %q", tg.Name(), v.Name(), want[tg.Name()])
			}
		}

		// TagTargets resolves them all in one call.
		targets, err := p.TagTargets(ctx)
		if err != nil {
			t.Fatalf("TagTargets: %v", err)
		}
		if diff := cmp.Diff(want, targets); diff != "" {
			t.Errorf("TagTargets (-want +got):\n%s", diff)
		}
	})
}

func TestTagTargetsEmpty(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		s, _ := NewWithBucket(b, testScope)
		targets, err := s.Package("requests").TagTargets(ctx)
		if err != nil {
			t.Fatalf("TagTargets (empty): %v", err)
		}
		if len(targets) != 0 {
			t.Errorf("TagTargets len = %d, want 0", len(targets))
		}
	})
}

func TestSetTagRaceConverges(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		s, _ := NewWithBucket(b, testScope)
		p := s.Package("requests")

		// N concurrent writers, each setting a distinct tag. Because every tag
		// is its own object, the writes are independent and none can be lost.
		const n = 24
		var wg sync.WaitGroup
		errs := make([]error, n)
		for i := 0; i < n; i++ {
			i := i
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = p.SetTag(ctx, fmt.Sprintf("tag-%02d", i), fmt.Sprintf("%d.0.0", i))
			}()
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("SetTag %d: %v", i, err)
			}
		}

		// Reconstruct the converged tag set by listing and resolving each tag.
		tags, err := p.Tags(ctx)
		if err != nil {
			t.Fatalf("Tags: %v", err)
		}
		got := make(map[string]string, len(tags))
		for _, tg := range tags {
			v, err := tg.Ref(ctx)
			if err != nil {
				t.Fatalf("Ref %q: %v", tg.Name(), err)
			}
			got[tg.Name()] = v.Name()
		}
		want := make(map[string]string, n)
		for i := 0; i < n; i++ {
			want[fmt.Sprintf("tag-%02d", i)] = fmt.Sprintf("%d.0.0", i)
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("converged tags (-want +got):\n%s", diff)
		}
	})
}

func TestBlobRedirectMissCachedAndSingleflight(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()

		// Gate the signer so the first caller blocks inside the flight until
		// every racer has joined. Without singleflight each goroutine would
		// reach the (still-blocked) signer and the count would climb to n;
		// with it, exactly one underlying sign happens. memblob/fileblob
		// report Unimplemented, which is recorded as an empty-URL miss.
		var signCalls atomic.Int64
		release := make(chan struct{})
		signer := func(ctx context.Context, key string, opts *blob.SignedURLOptions) (string, error) {
			signCalls.Add(1)
			<-release
			return b.SignedURL(ctx, key, opts)
		}
		s, _ := NewWithBucket(b, testScope, withSigner(signer))

		f := s.Package("p").Version("1.0.0").File("f.bin")
		if _, err := s.Package("p").Version("1.0.0").AddFile(ctx, "f.bin", bytes.NewReader([]byte("data"))); err != nil {
			t.Fatalf("AddFile: %v", err)
		}

		const n = 32
		var wg sync.WaitGroup
		urls := make([]string, n)
		errs := make([]error, n)
		for i := 0; i < n; i++ {
			i := i
			wg.Add(1)
			go func() {
				defer wg.Done()
				urls[i], errs[i] = f.DownloadURL(ctx)
			}()
		}
		// Let the racers coalesce onto the in-flight signer, then release it.
		time.Sleep(20 * time.Millisecond)
		close(release)
		wg.Wait()

		for i := 0; i < n; i++ {
			if errs[i] != nil {
				t.Fatalf("DownloadURL[%d]: %v", i, errs[i])
			}
			if urls[i] != "" {
				t.Errorf("DownloadURL[%d] = %q, want empty (no redirect support)", i, urls[i])
			}
		}
		// The miss is cached and singleflight collapses the racers: exactly one
		// underlying sign attempt despite n concurrent callers.
		if got := signCalls.Load(); got != 1 {
			t.Errorf("underlying sign called %d times, want 1", got)
		}

		// A later call is served from the cached miss — still no new sign.
		if u, err := f.DownloadURL(ctx); err != nil || u != "" {
			t.Fatalf("post-cache DownloadURL: got (%q, %v), want (\"\", nil)", u, err)
		}
		if got := signCalls.Load(); got != 1 {
			t.Errorf("after warm-cache call, sign count = %d, want 1", got)
		}
	})
}

func TestStatCacheCollapsesLookups(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()

		// Gate the statter the same way: a single in-flight lookup held open
		// until all racers join proves the cache+singleflight collapse them
		// into one backend call rather than relying on a fast warm-up.
		var statCalls atomic.Int64
		release := make(chan struct{})
		statter := func(ctx context.Context, key string) (*blob.Attributes, error) {
			statCalls.Add(1)
			<-release
			return b.Attributes(ctx, key)
		}
		s, _ := NewWithBucket(b, testScope, withStatter(statter))

		body := []byte("attributes payload")
		if _, err := s.Package("p").Version("1.0.0").AddFile(ctx, "f.bin", bytes.NewReader(body)); err != nil {
			t.Fatalf("AddFile: %v", err)
		}
		key := filePath(testScope, "p", "1.0.0", "f.bin")

		const n = 32
		var wg sync.WaitGroup
		sizes := make([]int64, n)
		errs := make([]error, n)
		for i := 0; i < n; i++ {
			i := i
			wg.Add(1)
			go func() {
				defer wg.Done()
				a, err := s.attributes(ctx, key)
				if err != nil {
					errs[i] = err
					return
				}
				sizes[i] = a.Size
			}()
		}
		time.Sleep(20 * time.Millisecond)
		close(release)
		wg.Wait()

		for i := 0; i < n; i++ {
			if errs[i] != nil {
				t.Fatalf("attributes[%d]: %v", i, errs[i])
			}
			if sizes[i] != int64(len(body)) {
				t.Errorf("attributes[%d].Size = %d, want %d", i, sizes[i], len(body))
			}
		}
		if got := statCalls.Load(); got != 1 {
			t.Errorf("underlying Attributes called %d times, want 1", got)
		}
	})
}

func TestPackageVersionMetaExistsAnnotate(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		s, _ := NewWithBucket(b, testScope)

		// Absent package.
		if ok, err := s.Package("p").Exists(ctx); err != nil || ok {
			t.Fatalf("Exists on absent package: got (%v, %v), want (false, nil)", ok, err)
		}
		if _, err := s.Package("p").Meta(ctx); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("Meta on absent package: got %v, want ErrNotFound", err)
		}

		// AddPackage twice without overwrite -> ErrAlreadyExists.
		if _, err := s.AddPackage(ctx, "p", core.WithAnnotations(map[string]any{"k": "v"})); err != nil {
			t.Fatalf("AddPackage: %v", err)
		}
		if _, err := s.AddPackage(ctx, "p"); !errors.Is(err, core.ErrAlreadyExists) {
			t.Fatalf("AddPackage dup: got %v, want ErrAlreadyExists", err)
		}

		p := s.Package("p")
		if ok, err := p.Exists(ctx); err != nil || !ok {
			t.Fatalf("Exists after AddPackage: got (%v, %v), want (true, nil)", ok, err)
		}
		meta, err := p.Meta(ctx)
		if err != nil {
			t.Fatalf("Meta: %v", err)
		}
		if diff := cmp.Diff(map[string]any{"k": "v"}, meta.Annotations); diff != "" {
			t.Errorf("package annotations (-want +got):\n%s", diff)
		}
		if meta.CreatedAt.IsZero() || meta.UpdatedAt.IsZero() {
			t.Errorf("expected timestamps set, got %+v", meta)
		}

		// Annotate replaces the annotations map and preserves CreatedAt.
		if err := p.Annotate(ctx, map[string]any{"k2": "v2"}); err != nil {
			t.Fatalf("Annotate: %v", err)
		}
		meta2, err := p.Meta(ctx)
		if err != nil {
			t.Fatalf("Meta after Annotate: %v", err)
		}
		if diff := cmp.Diff(map[string]any{"k2": "v2"}, meta2.Annotations); diff != "" {
			t.Errorf("annotations after Annotate (-want +got):\n%s", diff)
		}
		if !meta2.CreatedAt.Equal(meta.CreatedAt) {
			t.Errorf("CreatedAt changed: %v -> %v", meta.CreatedAt, meta2.CreatedAt)
		}

		// Version-level: AddVersion, Exists, Meta.
		if _, err := p.AddVersion(ctx, "1.0.0"); err != nil {
			t.Fatalf("AddVersion: %v", err)
		}
		if ok, err := p.Version("1.0.0").Exists(ctx); err != nil || !ok {
			t.Fatalf("Version Exists: got (%v, %v), want (true, nil)", ok, err)
		}
		// A version with only a file (no .meta) still Exists via descendant.
		if _, err := p.Version("2.0.0").AddFile(ctx, "f.whl", bytes.NewReader([]byte("x"))); err != nil {
			t.Fatalf("AddFile: %v", err)
		}
		if ok, err := p.Version("2.0.0").Exists(ctx); err != nil || !ok {
			t.Fatalf("Version Exists via descendant: got (%v, %v), want (true, nil)", ok, err)
		}
	})
}

func TestSetTagCreatesTagObject(t *testing.T) {
	t.Parallel()

	b := memblob.OpenBucket(nil)
	defer b.Close()
	ctx := t.Context()
	s, _ := NewWithBucket(b, testScope)

	// The tag object must not exist before the first SetTag.
	if ok, err := b.Exists(ctx, tagPath(testScope, "fresh", "latest")); err != nil || ok {
		t.Fatalf("tag pre-existence: got (%v, %v), want (false, nil)", ok, err)
	}
	if err := s.Package("fresh").SetTag(ctx, "latest", "1.0.0"); err != nil {
		t.Fatalf("SetTag: %v", err)
	}
	if ok, err := b.Exists(ctx, tagPath(testScope, "fresh", "latest")); err != nil || !ok {
		t.Fatalf("tag post-existence: got (%v, %v), want (true, nil)", ok, err)
	}
	// The object content is the bare target version string.
	raw, err := b.ReadAll(ctx, tagPath(testScope, "fresh", "latest"))
	if err != nil {
		t.Fatalf("read tag object: %v", err)
	}
	if got := string(raw); got != "1.0.0" {
		t.Errorf("tag object content = %q, want %q", got, "1.0.0")
	}
	v, err := s.Package("fresh").Tag("latest").Ref(ctx)
	if err != nil {
		t.Fatalf("Ref: %v", err)
	}
	if v.Name() != "1.0.0" {
		t.Errorf("Ref = %q, want %q", v.Name(), "1.0.0")
	}
}
