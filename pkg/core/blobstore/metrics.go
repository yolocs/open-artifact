package blobstore

import (
	"context"
	"time"

	"gocloud.dev/blob"
	"gocloud.dev/gcerrors"
)

// Metrics observes backend blob operations and download-redirect decisions. It
// is a deliberately small, metrics-agnostic hook — the same shape as Guard —
// so blobstore depends on this local interface rather than pkg/metrics, keeping
// pkg/core free of a metrics-library import. A *metrics.Prometheus (or any
// metrics.Recorder) satisfies it structurally.
type Metrics interface {
	BlobStoreCall(op, status string, duration time.Duration)
	BlobRedirect(outcome string)
}

// WithMetrics installs a Metrics observer. A nil observer (the default) leaves
// the Store uninstrumented; every emission is guarded by a nil check.
func WithMetrics(m Metrics) Option {
	return func(s *Store) { s.metrics = m }
}

// Blob operation labels. They name the bucket primitive being timed.
const (
	opExists      = "exists"
	opReadAll     = "read_all"
	opWriteAll    = "write_all"
	opNewReader   = "new_reader"
	opNewWriter   = "new_writer"
	opWriterClose = "writer_close"
	opList        = "list"
	opAttributes  = "attributes"
	opSignedURL   = "signed_url"
	opDelete      = "delete"
)

// observe records one backend call, classifying err into a stable status. It is
// a no-op when no Metrics observer is installed.
func (s *Store) observe(op string, start time.Time, err error) {
	if s.metrics == nil {
		return
	}
	s.metrics.BlobStoreCall(op, blobStatus(err), time.Since(start))
}

// redirect records a download-URL decision, if a Metrics observer is installed.
func (s *Store) redirect(outcome string) {
	if s.metrics != nil {
		s.metrics.BlobRedirect(outcome)
	}
}

// blobStatus maps a backend error to a stable metric status. It reads the
// gocloud error code (before sentinel mapping) so the classification is the
// same across backends.
func blobStatus(err error) string {
	switch gcerrors.Code(err) {
	case gcerrors.OK:
		return "ok"
	case gcerrors.NotFound:
		return "not_found"
	case gcerrors.AlreadyExists:
		return "already_exists"
	case gcerrors.Unimplemented:
		return "unsupported"
	default:
		return "error"
	}
}

// The b* methods wrap the bucket primitives with timing + status emission. They
// return the raw bucket error unchanged; callers keep their existing wrapping
// and sentinel mapping.

func (s *Store) bExists(ctx context.Context, key string) (bool, error) {
	start := time.Now()
	ok, err := s.bucket.Exists(ctx, key)
	s.observe(opExists, start, err)
	return ok, err
}

func (s *Store) bReadAll(ctx context.Context, key string) ([]byte, error) {
	start := time.Now()
	b, err := s.bucket.ReadAll(ctx, key)
	s.observe(opReadAll, start, err)
	return b, err
}

func (s *Store) bWriteAll(ctx context.Context, key string, p []byte, opts *blob.WriterOptions) error {
	start := time.Now()
	err := s.bucket.WriteAll(ctx, key, p, opts)
	s.observe(opWriteAll, start, err)
	return err
}

func (s *Store) bNewReader(ctx context.Context, key string, opts *blob.ReaderOptions) (*blob.Reader, error) {
	start := time.Now()
	r, err := s.bucket.NewReader(ctx, key, opts)
	s.observe(opNewReader, start, err)
	return r, err
}

func (s *Store) bNewWriter(ctx context.Context, key string, opts *blob.WriterOptions) (*blob.Writer, error) {
	start := time.Now()
	w, err := s.bucket.NewWriter(ctx, key, opts)
	s.observe(opNewWriter, start, err)
	return w, err
}

func (s *Store) bDelete(ctx context.Context, key string) error {
	start := time.Now()
	err := s.bucket.Delete(ctx, key)
	s.observe(opDelete, start, err)
	return err
}

// closeWriter times and records the writer's Close, returning its error.
func (s *Store) closeWriter(w *blob.Writer) error {
	start := time.Now()
	err := w.Close()
	s.observe(opWriterClose, start, err)
	return err
}
