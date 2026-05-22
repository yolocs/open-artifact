package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yolocs/open-artifact/pkg/logging"
)

// fakeRecorder captures HTTPRequest calls for assertions.
type fakeRecorder struct {
	mu    sync.Mutex
	calls []httpCall
}

type httpCall struct {
	format, op, status string
	bytesIn, bytesOut  int64
}

func (f *fakeRecorder) HTTPRequest(format, op, status string, _ time.Duration, bytesIn, bytesOut int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, httpCall{format, op, status, bytesIn, bytesOut})
}
func (f *fakeRecorder) BlobStoreCall(string, string, time.Duration) {}
func (f *fakeRecorder) BlobRedirect(string)                         {}

func (f *fakeRecorder) last(t *testing.T) httpCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatal("no HTTPRequest calls recorded")
	}
	return f.calls[len(f.calls)-1]
}

// clock is a manual time source for the readiness cache.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func okPinger(context.Context) error { return nil }

func do(t *testing.T, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestLivenessGET(t *testing.T) {
	t.Parallel()
	h := Wrap(Config{Next: http.NotFoundHandler(), Component: "test"})
	rec := do(t, h, http.MethodGet, "/healthz")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "ok\n" {
		t.Errorf("body = %q, want %q", got, "ok\n")
	}
}

func TestLivenessHEAD(t *testing.T) {
	t.Parallel()
	h := Wrap(Config{Next: http.NotFoundHandler(), Component: "test"})
	rec := do(t, h, http.MethodHead, "/healthz")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD body = %q, want empty", rec.Body.String())
	}
}

func TestReadyzSuccess(t *testing.T) {
	t.Parallel()
	h := Wrap(Config{Next: http.NotFoundHandler(), Pinger: okPinger, Component: "test"})
	rec := do(t, h, http.MethodGet, "/readyz")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body readyzBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Status != "ok" || body.Backend != "ok" {
		t.Errorf("status/backend = %q/%q, want ok/ok", body.Status, body.Backend)
	}
	if body.Build.OSArch == "" || body.Build.Version == "" {
		t.Errorf("build identity not reported: %+v", body.Build)
	}
	if body.Error != "" {
		t.Errorf("error = %q, want empty on success", body.Error)
	}
}

func TestReadyzHEAD(t *testing.T) {
	t.Parallel()
	h := Wrap(Config{Next: http.NotFoundHandler(), Pinger: okPinger, Component: "test"})
	rec := do(t, h, http.MethodHead, "/readyz")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD body = %q, want empty", rec.Body.String())
	}
}

func TestReadyzBackendFailure(t *testing.T) {
	t.Parallel()
	pinger := func(context.Context) error { return errors.New("bucket unreachable: s3://secret-bucket") }
	h := Wrap(Config{Next: http.NotFoundHandler(), Pinger: pinger, Component: "test"})
	rec := do(t, h, http.MethodGet, "/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	var body readyzBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Status != "error" || body.Backend != "error" {
		t.Errorf("status/backend = %q/%q, want error/error", body.Status, body.Backend)
	}
	if body.Error == "" {
		t.Error("error field should be set on failure")
	}
	if strings.Contains(rec.Body.String(), "secret-bucket") {
		t.Errorf("readyz body leaked backend detail: %q", rec.Body.String())
	}
}

func TestReadyzNoPingerCollapsesToLiveness(t *testing.T) {
	t.Parallel()
	h := Wrap(Config{Next: http.NotFoundHandler(), Component: "test"}) // nil pinger
	rec := do(t, h, http.MethodGet, "/readyz")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (readiness collapses to liveness)", rec.Code)
	}
}

func TestReadyzSuccessCachedNotProbeStorm(t *testing.T) {
	t.Parallel()
	var calls int32
	pinger := func(context.Context) error {
		atomic.AddInt32(&calls, 1)
		return nil
	}
	clk := &clock{t: time.Unix(1700000000, 0)}
	h := Wrap(Config{Next: http.NotFoundHandler(), Pinger: pinger, Component: "test", now: clk.now})

	do(t, h, http.MethodGet, "/readyz")
	do(t, h, http.MethodGet, "/readyz") // within 1s cache window
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("pinger called %d times within cache window, want 1", got)
	}

	clk.advance(2 * time.Second) // past the 1s success cache
	do(t, h, http.MethodGet, "/readyz")
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("pinger called %d times after cache expiry, want 2", got)
	}
}

func TestReadyzFailureNotCached(t *testing.T) {
	t.Parallel()
	var calls int32
	pinger := func(context.Context) error {
		atomic.AddInt32(&calls, 1)
		return errors.New("down")
	}
	clk := &clock{t: time.Unix(1700000000, 0)}
	h := Wrap(Config{Next: http.NotFoundHandler(), Pinger: pinger, Component: "test", now: clk.now})

	do(t, h, http.MethodGet, "/readyz")
	do(t, h, http.MethodGet, "/readyz")
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("failed probe should not be cached: pinger called %d times, want 2", got)
	}
}

func TestMetricsDisabledRouting(t *testing.T) {
	t.Parallel()
	h := Wrap(Config{Next: http.NotFoundHandler(), MetricsPath: "/metrics", Component: "test"}) // nil MetricsHandler
	rec := do(t, h, http.MethodGet, "/metrics")
	if rec.Code != http.StatusNotFound {
		t.Errorf("disabled metrics path status = %d, want 404", rec.Code)
	}
}

func TestMetricsEnabledRouting(t *testing.T) {
	t.Parallel()
	served := false
	metricsHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("# metrics"))
	})
	h := Wrap(Config{Next: http.NotFoundHandler(), MetricsHandler: metricsHandler, MetricsPath: "/metrics", Component: "test"})
	rec := do(t, h, http.MethodGet, "/metrics")
	if rec.Code != http.StatusOK || !served {
		t.Errorf("enabled metrics path status = %d served = %v, want 200/true", rec.Code, served)
	}
}

func TestObserveStatusDefaultsTo200(t *testing.T) {
	t.Parallel()
	rec := &fakeRecorder{}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Writes a body without calling WriteHeader.
		_, _ = w.Write([]byte("hello"))
	})
	h := Wrap(Config{Next: next, Recorder: rec, Component: "test"})

	resp := do(t, h, http.MethodGet, "/anything")
	if resp.Code != http.StatusOK {
		t.Errorf("response status = %d, want 200", resp.Code)
	}
	c := rec.last(t)
	if c.status != "200" {
		t.Errorf("recorded status = %q, want 200", c.status)
	}
	if c.bytesOut != int64(len("hello")) {
		t.Errorf("bytesOut = %d, want %d", c.bytesOut, len("hello"))
	}
	if c.op != "read" {
		t.Errorf("op = %q, want read (GET fallback)", c.op)
	}
	if c.format != "unknown" {
		t.Errorf("format = %q, want unknown (default)", c.format)
	}
}

func TestObserveOpFallbackByMethod(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		http.MethodGet:    "read",
		http.MethodHead:   "read",
		http.MethodPut:    "write",
		http.MethodPost:   "write",
		http.MethodPatch:  "write",
		http.MethodDelete: "delete",
		"OPTIONS":         "options",
	}
	for method, wantOp := range cases {
		method, wantOp := method, wantOp
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			rec := &fakeRecorder{}
			h := Wrap(Config{Next: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}), Recorder: rec, Component: "test"})
			do(t, h, method, "/x")
			if got := rec.last(t).op; got != wantOp {
				t.Errorf("op = %q, want %q", got, wantOp)
			}
		})
	}
}

func TestObserveExplicitOp(t *testing.T) {
	t.Parallel()
	rec := &fakeRecorder{}
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		SetOperation(r, "download")
	})
	h := Wrap(Config{Next: next, Recorder: rec, Component: "test"})
	do(t, h, http.MethodGet, "/x")
	if got := rec.last(t).op; got != "download" {
		t.Errorf("op = %q, want download (explicit overrides fallback)", got)
	}
}

func TestObserveFormatPropagatesThroughNestedRouter(t *testing.T) {
	t.Parallel()
	rec := &fakeRecorder{}
	inner := http.NewServeMux()
	inner.HandleFunc("GET /pkg", func(w http.ResponseWriter, r *http.Request) {
		SetOperation(r, "list")
		w.WriteHeader(http.StatusOK)
	})
	// WrapWithFormat sits between observe and the inner router, the way a
	// format surface is mounted.
	h := Wrap(Config{Next: WrapWithFormat("pypi", inner), Recorder: rec, Component: "test"})
	do(t, h, http.MethodGet, "/pkg")
	c := rec.last(t)
	if c.format != "pypi" {
		t.Errorf("format = %q, want pypi", c.format)
	}
	if c.op != "list" {
		t.Errorf("op = %q, want list", c.op)
	}
}

func TestObserveBytesInFromContentLength(t *testing.T) {
	t.Parallel()
	rec := &fakeRecorder{}
	h := Wrap(Config{Next: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}), Recorder: rec, Component: "test"})
	body := strings.NewReader("payload-bytes")
	req := httptest.NewRequest(http.MethodPut, "/upload", body)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got := rec.last(t).bytesIn; got != int64(len("payload-bytes")) {
		t.Errorf("bytesIn = %d, want %d", got, len("payload-bytes"))
	}
}

func TestRequestLogDoesNotLogAuthorization(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger, err := logging.New(&buf, logging.Options{Level: "info", Format: "json"})
	if err != nil {
		t.Fatalf("logging.New: %v", err)
	}

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		SetNamespace(r, "team-a")
		w.WriteHeader(http.StatusOK)
	})
	h := Wrap(Config{Next: next, Recorder: &fakeRecorder{}, Component: "serve"})

	const secret = "super-secret-token"
	req := httptest.NewRequest(http.MethodGet, "/team-a/echo", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("X-Request-Id", "req-123")
	req = req.WithContext(logging.ContextWithLogger(context.Background(), logger))
	h.ServeHTTP(httptest.NewRecorder(), req)

	out := buf.String()
	if out == "" {
		t.Fatal("expected a request log line")
	}
	if strings.Contains(out, secret) || strings.Contains(out, "Authorization") {
		t.Errorf("request log leaked authorization: %s", out)
	}
	for _, want := range []string{`"method":"GET"`, `"status":200`, `"namespace":"team-a"`, `"request_id":"req-123"`} {
		if !strings.Contains(out, want) {
			t.Errorf("request log missing %s; got %s", want, out)
		}
	}
}

func TestRequestLogRecordsError(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger, err := logging.New(&buf, logging.Options{Level: "info", Format: "json"})
	if err != nil {
		t.Fatalf("logging.New: %v", err)
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		RecordError(r, errors.New("boom"))
		w.WriteHeader(http.StatusInternalServerError)
	})
	h := Wrap(Config{Next: next, Recorder: &fakeRecorder{}, Component: "serve"})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req = req.WithContext(logging.ContextWithLogger(context.Background(), logger))
	h.ServeHTTP(httptest.NewRecorder(), req)

	out := buf.String()
	if !strings.Contains(out, `"error":"boom"`) {
		t.Errorf("expected error field in log; got %s", out)
	}
}
