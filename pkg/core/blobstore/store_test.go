package blobstore

import (
	"bytes"
	"crypto"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"gocloud.dev/blob"
	"gocloud.dev/blob/fileblob"
	"gocloud.dev/blob/memblob"

	"github.com/yolocs/open-artifact/pkg/core"
)

const testScope = "pypi/global"

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

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// failAfterReader yields n bytes then fails, simulating a body whose source
// (an upstream stream, a disconnected client) errors mid-transfer.
type failAfterReader struct {
	data []byte
	pos  int
	at   int
}

func (r *failAfterReader) Read(p []byte) (int, error) {
	if r.pos >= r.at {
		return 0, errors.New("mid-stream failure")
	}
	n := copy(p, r.data[r.pos:r.at])
	r.pos += n
	return n, nil
}

// TestAddFileMidStreamErrorLeavesNoPartialBlob proves that a body that errors
// part-way through does not leave a partial, servable File: AddFile reports the
// error and the File must not exist afterward. Without aborting the writer this
// regresses to a poisoned cache entry (Exists true, Read returns truncated bytes
// because the sidecar never landed and verification is then skipped).
func TestAddFileMidStreamErrorLeavesNoPartialBlob(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		s, err := NewWithBucket(b, testScope)
		if err != nil {
			t.Fatalf("NewWithBucket: %v", err)
		}
		body := &failAfterReader{data: []byte("0123456789abcdefghij"), at: 5}
		v := s.Package("demo").Version("1.0.0")
		if _, err := v.AddFile(ctx, "demo-1.0.0-py3-none-any.whl", body); err == nil {
			t.Fatal("AddFile succeeded on a mid-stream error, want failure")
		}

		f := v.File("demo-1.0.0-py3-none-any.whl")
		exists, err := f.Exists(ctx)
		if err != nil {
			t.Fatalf("Exists: %v", err)
		}
		if exists {
			t.Fatal("partial blob left after mid-stream error: File.Exists = true, want false")
		}
	})
}

func TestAddFileReadFileRoundTrip(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		s, err := NewWithBucket(b, testScope)
		if err != nil {
			t.Fatalf("NewWithBucket: %v", err)
		}

		body := []byte("the quick brown fox\n")
		v := s.Package("requests").Version("2.31.0")
		f, err := v.AddFile(ctx, "requests-2.31.0.whl", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("AddFile: %v", err)
		}

		rc, err := f.Read(ctx)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		got, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if err := rc.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if !bytes.Equal(got, body) {
			t.Errorf("round-trip bytes mismatch: got %q want %q", got, body)
		}

		meta, err := f.Meta(ctx)
		if err != nil {
			t.Fatalf("Meta: %v", err)
		}
		if want := sha256Hex(body); meta.Digest != want {
			t.Errorf("digest = %q, want %q", meta.Digest, want)
		}
		if meta.Size != int64(len(body)) {
			t.Errorf("size = %d, want %d", meta.Size, len(body))
		}
	})
}

func TestAddFileVerifiesExpectedDigests(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		s, err := NewWithBucket(b, testScope)
		if err != nil {
			t.Fatalf("NewWithBucket: %v", err)
		}
		body := []byte("verify me")
		sum := sha1.Sum(body)

		// A matching declared digest commits normally and is recorded in
		// Meta.Digests (the canonical SHA-256 stays in Meta.Digest).
		f, err := s.Package("p").Version("1.0.0").AddFile(ctx, "ok.bin", bytes.NewReader(body),
			core.WithExpectedDigests(core.ExpectedDigest{Hash: crypto.SHA1, Sum: sum[:]}))
		if err != nil {
			t.Fatalf("AddFile (matching digest): %v", err)
		}
		meta, err := f.Meta(ctx)
		if err != nil {
			t.Fatalf("Meta: %v", err)
		}
		if got, want := meta.Digests["sha1"], hex.EncodeToString(sum[:]); got != want {
			t.Fatalf("Meta.Digests[sha1] = %q, want %q", got, want)
		}
		if _, ok := meta.Digests["sha256"]; ok {
			t.Fatalf("Meta.Digests should exclude canonical sha256: %v", meta.Digests)
		}

		// A mismatch aborts with ErrDigestMismatch and commits nothing.
		bad := s.Package("p").Version("2.0.0").File("bad.bin")
		_, err = s.Package("p").Version("2.0.0").AddFile(ctx, "bad.bin", bytes.NewReader(body),
			core.WithExpectedDigests(core.ExpectedDigest{Hash: crypto.SHA1, Sum: make([]byte, 20)}))
		if !errors.Is(err, core.ErrDigestMismatch) {
			t.Fatalf("AddFile (mismatch) err = %v, want ErrDigestMismatch", err)
		}
		if exists, _ := bad.Exists(ctx); exists {
			t.Fatalf("mismatched upload left a committed blob")
		}
	})
}

func TestAddFilePersistsDigestInSidecar(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		s, _ := NewWithBucket(b, testScope)

		body := []byte("payload bytes for digest check")
		_, err := s.Package("p").Version("1.0.0").AddFile(ctx, "f.txt", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("AddFile: %v", err)
		}

		// Read the sidecar directly: the digest must already be there,
		// proving it was computed during streaming, not re-derived on read.
		raw, err := b.ReadAll(ctx, fileMetaPath(testScope, "p", "1.0.0", "f.txt"))
		if err != nil {
			t.Fatalf("read sidecar: %v", err)
		}
		meta, err := decodeMeta(raw)
		if err != nil {
			t.Fatalf("decodeMeta: %v", err)
		}
		if want := sha256Hex(body); meta.Digest != want {
			t.Errorf("sidecar digest = %q, want %q", meta.Digest, want)
		}
		if meta.CreatedAt.IsZero() || meta.UpdatedAt.IsZero() {
			t.Errorf("expected timestamps to be set, got %+v", meta)
		}
	})
}

func TestAddFileOverwrite(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		s, _ := NewWithBucket(b, testScope)
		v := s.Package("p").Version("1.0.0")

		if _, err := v.AddFile(ctx, "f.txt", bytes.NewReader([]byte("first"))); err != nil {
			t.Fatalf("first AddFile: %v", err)
		}

		// Default (AllowOverwrite=false) rejects the clobber.
		_, err := v.AddFile(ctx, "f.txt", bytes.NewReader([]byte("second")))
		if !errors.Is(err, core.ErrAlreadyExists) {
			t.Fatalf("expected ErrAlreadyExists, got %v", err)
		}

		// AllowOverwrite=true is last-write-wins.
		f, err := v.AddFile(ctx, "f.txt", bytes.NewReader([]byte("second")), core.WithAllowOverwrite(true))
		if err != nil {
			t.Fatalf("overwrite AddFile: %v", err)
		}
		rc, _ := f.Read(ctx)
		got, _ := io.ReadAll(rc)
		rc.Close()
		if string(got) != "second" {
			t.Errorf("after overwrite got %q, want %q", got, "second")
		}
		meta, _ := f.Meta(ctx)
		if want := sha256Hex([]byte("second")); meta.Digest != want {
			t.Errorf("after overwrite digest = %q, want %q", meta.Digest, want)
		}
	})
}

func TestConcurrentAddFileDifferentFiles(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		s, _ := NewWithBucket(b, testScope)
		v := s.Package("p").Version("1.0.0")

		const n = 16
		var wg sync.WaitGroup
		errs := make([]error, n)
		for i := 0; i < n; i++ {
			i := i
			wg.Add(1)
			go func() {
				defer wg.Done()
				name := fmt.Sprintf("file-%02d.txt", i)
				body := bytes.Repeat([]byte{byte('a' + i)}, 100+i)
				_, errs[i] = v.AddFile(ctx, name, bytes.NewReader(body))
			}()
		}
		wg.Wait()

		for i, err := range errs {
			if err != nil {
				t.Errorf("AddFile %d: %v", i, err)
			}
		}
		// Every file must be independently readable with the right bytes.
		for i := 0; i < n; i++ {
			name := fmt.Sprintf("file-%02d.txt", i)
			want := bytes.Repeat([]byte{byte('a' + i)}, 100+i)
			rc, err := v.File(name).Read(ctx)
			if err != nil {
				t.Fatalf("Read %s: %v", name, err)
			}
			got, _ := io.ReadAll(rc)
			rc.Close()
			if !bytes.Equal(got, want) {
				t.Errorf("file %s corrupted", name)
			}
		}
	})
}

func TestAddReadRaceSameFile(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		s, _ := NewWithBucket(b, testScope)
		v := s.Package("p").Version("1.0.0")

		body := bytes.Repeat([]byte("xyz"), 1000)
		if _, err := v.AddFile(ctx, "f.bin", bytes.NewReader(body)); err != nil {
			t.Fatalf("seed AddFile: %v", err)
		}

		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(2)
			go func() {
				defer wg.Done()
				_, _ = v.AddFile(ctx, "f.bin", bytes.NewReader(body), core.WithAllowOverwrite(true))
			}()
			go func() {
				defer wg.Done()
				rc, err := v.File("f.bin").Read(ctx)
				if err != nil {
					return
				}
				got, _ := io.ReadAll(rc)
				rc.Close()
				// Whatever wins, the bytes must never be a torn mix.
				if len(got) != 0 && !bytes.Equal(got, body) {
					t.Errorf("torn read: len=%d", len(got))
				}
			}()
		}
		wg.Wait()
	})
}

func TestMetaRecomputedWhenSidecarMissing(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		s, _ := NewWithBucket(b, testScope)
		v := s.Package("p").Version("1.0.0")

		body := []byte("recompute me")
		f, err := v.AddFile(ctx, "f.txt", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("AddFile: %v", err)
		}

		// Delete the sidecar; Meta must recompute the digest from the blob.
		if err := b.Delete(ctx, fileMetaPath(testScope, "p", "1.0.0", "f.txt")); err != nil {
			t.Fatalf("delete sidecar: %v", err)
		}
		meta, err := f.Meta(ctx)
		if err != nil {
			t.Fatalf("Meta after sidecar delete: %v", err)
		}
		if want := sha256Hex(body); meta.Digest != want {
			t.Errorf("recomputed digest = %q, want %q", meta.Digest, want)
		}
		if meta.Size != int64(len(body)) {
			t.Errorf("recomputed size = %d, want %d", meta.Size, len(body))
		}
	})
}

func TestMetaRecomputedWhenSidecarCorrupted(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		s, _ := NewWithBucket(b, testScope)
		v := s.Package("p").Version("1.0.0")

		body := []byte("corrupt sidecar case")
		f, err := v.AddFile(ctx, "f.txt", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("AddFile: %v", err)
		}

		if err := b.WriteAll(ctx, fileMetaPath(testScope, "p", "1.0.0", "f.txt"), []byte("{garbage"), nil); err != nil {
			t.Fatalf("corrupt sidecar: %v", err)
		}
		meta, err := f.Meta(ctx)
		if err != nil {
			t.Fatalf("Meta with corrupt sidecar: %v", err)
		}
		if want := sha256Hex(body); meta.Digest != want {
			t.Errorf("recomputed digest = %q, want %q", meta.Digest, want)
		}
	})
}

func TestReadNotFound(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		s, _ := NewWithBucket(b, testScope)
		_, err := s.Package("nope").Version("0.0.0").File("missing.txt").Read(ctx)
		if !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("expected ErrNotFound, got %v", err)
		}
	})
}

func TestFileblobAcceptsLeadingDotPaths(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	b, err := fileblob.OpenBucket(dir, nil)
	if err != nil {
		t.Fatalf("fileblob.OpenBucket: %v", err)
	}
	defer b.Close()

	ctx := t.Context()
	// The sidecar path contains a leading-dot segment (".meta.<file>").
	key := fileMetaPath(testScope, "p", "1.0.0", "f.txt")
	if !strings.Contains(key, "/.meta.") {
		t.Fatalf("sidecar key %q missing leading-dot segment", key)
	}
	if err := b.WriteAll(ctx, key, []byte("{}"), nil); err != nil {
		t.Fatalf("fileblob rejected leading-dot path %q: %v", key, err)
	}
	if _, err := b.ReadAll(ctx, key); err != nil {
		t.Fatalf("fileblob read-back of %q: %v", key, err)
	}
}

func TestStoreUsesInjectedClock(t *testing.T) {
	t.Parallel()

	b := memblob.OpenBucket(nil)
	defer b.Close()

	fixed := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	s, _ := NewWithBucket(b, testScope, withClock(func() time.Time { return fixed }))

	ctx := t.Context()
	f, err := s.Package("p").Version("1.0.0").AddFile(ctx, "f.txt", bytes.NewReader([]byte("x")))
	if err != nil {
		t.Fatalf("AddFile: %v", err)
	}
	meta, _ := f.Meta(ctx)
	if !meta.CreatedAt.Equal(fixed) || !meta.UpdatedAt.Equal(fixed) {
		t.Errorf("timestamps = %v / %v, want %v", meta.CreatedAt, meta.UpdatedAt, fixed)
	}
}
