package blobstore

import (
	"bytes"
	"io"
	"sync"
	"testing"
	"time"

	"gocloud.dev/blob"
)

// fakeMetrics captures blob-backend calls and redirect outcomes for assertions.
type fakeMetrics struct {
	mu        sync.Mutex
	calls     map[string]int
	statuses  map[string]string
	redirects map[string]int
}

func newFakeMetrics() *fakeMetrics {
	return &fakeMetrics{
		calls:     map[string]int{},
		statuses:  map[string]string{},
		redirects: map[string]int{},
	}
}

func (f *fakeMetrics) BlobStoreCall(op, status string, _ time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[op]++
	f.statuses[op] = status
}

func (f *fakeMetrics) BlobRedirect(outcome string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.redirects[outcome]++
}

func (f *fakeMetrics) count(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[op]
}

func (f *fakeMetrics) status(op string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.statuses[op]
}

func (f *fakeMetrics) redirect(outcome string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.redirects[outcome]
}

func TestMetricsEmittedAcrossFileLifecycle(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		fm := newFakeMetrics()
		s, err := NewWithBucket(b, testScope, WithMetrics(fm))
		if err != nil {
			t.Fatalf("NewWithBucket: %v", err)
		}

		body := []byte("metrics observe this\n")
		v := s.Package("requests").Version("2.31.0")

		// AddFile: existence probe, writer open, writer close, sidecar write.
		f, err := v.AddFile(ctx, "requests-2.31.0.whl", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("AddFile: %v", err)
		}

		// Read: reader open + sidecar digest read.
		rc, err := f.Read(ctx)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if _, err := io.Copy(io.Discard, rc); err != nil {
			t.Fatalf("read body: %v", err)
		}
		if err := rc.Close(); err != nil {
			t.Fatalf("close reader: %v", err)
		}

		// Meta: sidecar read.
		if _, err := f.Meta(ctx); err != nil {
			t.Fatalf("Meta: %v", err)
		}

		// DownloadURL: signing unsupported on mem/file backends -> inline.
		url, err := f.DownloadURL(ctx)
		if err != nil {
			t.Fatalf("DownloadURL: %v", err)
		}
		if url != "" {
			t.Fatalf("expected empty signed URL on unsigned backend, got %q", url)
		}

		for _, op := range []string{opExists, opNewWriter, opWriterClose, opWriteAll, opNewReader, opReadAll, opSignedURL} {
			if fm.count(op) == 0 {
				t.Errorf("expected at least one %q call, got none", op)
			}
		}
		if got := fm.status(opWriteAll); got != "ok" {
			t.Errorf("write_all status = %q, want ok", got)
		}
		if got := fm.status(opSignedURL); got != "unsupported" {
			t.Errorf("signed_url status = %q, want unsupported", got)
		}
		if fm.redirect("inline") == 0 {
			t.Errorf("expected an inline redirect outcome, got %v", fm.redirects)
		}
		if fm.redirect("redirected") != 0 {
			t.Errorf("unsigned backend should not report redirected: %v", fm.redirects)
		}
	})
}

func TestMetricsListAndNotFound(t *testing.T) {
	t.Parallel()

	eachBackend(t, func(t *testing.T, b *blob.Bucket) {
		ctx := t.Context()
		fm := newFakeMetrics()
		s, err := NewWithBucket(b, testScope, WithMetrics(fm))
		if err != nil {
			t.Fatalf("NewWithBucket: %v", err)
		}

		if _, err := s.Packages(ctx); err != nil {
			t.Fatalf("Packages: %v", err)
		}
		if fm.count(opList) == 0 {
			t.Errorf("expected a list call from Packages")
		}

		// Reading a missing meta classifies as not_found.
		if _, err := s.Package("absent").Meta(ctx); err == nil {
			t.Fatal("expected ErrNotFound reading absent package meta")
		}
		if got := fm.status(opReadAll); got != "not_found" {
			t.Errorf("read_all status = %q, want not_found", got)
		}
	})
}
