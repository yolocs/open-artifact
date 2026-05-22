// Package observability is the cross-cutting HTTP wrapper shared by the data
// and admin planes. It intercepts the liveness, readiness, and metrics
// endpoints before auth or format routing, records per-request metrics and
// structured logs around the inner routes, and exposes small helpers
// (WrapWithFormat, SetOperation, SetNamespace) so inner routers can label a
// request with its format and operation.
//
// It depends on pkg/metrics, pkg/logging, and internal/version — never on a
// surface or on pkg/core — so it can wrap any plane without coupling to a
// protocol.
package observability

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yolocs/open-artifact/internal/version"
	"github.com/yolocs/open-artifact/pkg/logging"
	"github.com/yolocs/open-artifact/pkg/metrics"
)

const (
	// readyTimeout bounds a single backend readiness probe.
	readyTimeout = 2 * time.Second
	// readyCacheTTL is how long a successful probe is cached to avoid probe
	// storms. Failed probes are not cached.
	readyCacheTTL = 1 * time.Second
)

// Pinger probes backend readiness. It returns nil when the backend is reachable
// and an error otherwise. The data plane proves the bucket is reachable; the
// admin plane proves namespace metadata is listable. A nil Pinger collapses
// readiness to liveness.
type Pinger func(ctx context.Context) error

// Config wires the observability wrapper.
type Config struct {
	// Next is the plane's inner handler (format or admin routes). It is wrapped
	// with request metrics and logging.
	Next http.Handler
	// Recorder records HTTP metrics. A nil Recorder defaults to metrics.NoOp.
	Recorder metrics.Recorder
	// MetricsHandler serves the exposition endpoint. A nil handler disables the
	// endpoint: the configured path returns 404.
	MetricsHandler http.Handler
	// MetricsPath is the path the metrics endpoint is served on (default
	// "/metrics").
	MetricsPath string
	// Pinger probes backend readiness; nil collapses readiness to liveness.
	Pinger Pinger
	// Component labels logs from this plane (e.g. "serve", "admin").
	Component string

	// now is a test seam for the readiness cache and request timing; nil uses
	// time.Now.
	now func() time.Time
}

// Wrap builds the observability handler around cfg.Next.
func Wrap(cfg Config) http.Handler {
	now := cfg.now
	if now == nil {
		now = time.Now
	}
	rec := cfg.Recorder
	if rec == nil {
		rec = metrics.NoOp()
	}
	o := &obs{
		recorder:  rec,
		prober:    newProber(cfg.Pinger, now),
		build:     version.Get(),
		component: cfg.Component,
		now:       now,
	}

	path := cfg.MetricsPath
	if path == "" {
		path = "/metrics"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", o.liveness)
	mux.HandleFunc("HEAD /healthz", o.livenessHead)
	mux.HandleFunc("GET /readyz", o.readyz)
	mux.HandleFunc("HEAD /readyz", o.readyzHead)
	if cfg.MetricsHandler != nil {
		mux.Handle("GET "+path, cfg.MetricsHandler)
	} else {
		mux.HandleFunc("GET "+path, http.NotFound)
	}
	mux.Handle("/", o.observe(cfg.Next))
	return mux
}

type obs struct {
	recorder  metrics.Recorder
	prober    *prober
	build     version.Build
	component string
	now       func() time.Time
}

func (o *obs) liveness(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok\n")
}

func (o *obs) livenessHead(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func (o *obs) readyz(w http.ResponseWriter, r *http.Request) {
	body, status := o.readiness(r)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (o *obs) readyzHead(w http.ResponseWriter, r *http.Request) {
	_, status := o.readiness(r)
	w.WriteHeader(status)
}

// readyzBody is the readiness response body.
type readyzBody struct {
	Status  string    `json:"status"`
	Backend string    `json:"backend"`
	Build   buildInfo `json:"build"`
	Error   string    `json:"error,omitempty"`
}

type buildInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	OSArch  string `json:"os_arch"`
}

// readiness probes the backend and builds the response body and status. On
// failure it returns a generic, secret-free error message and logs the real
// cause.
func (o *obs) readiness(r *http.Request) (readyzBody, int) {
	build := buildInfo{Version: o.build.Version, Commit: o.build.Commit, OSArch: o.build.OSArch}
	if err := o.prober.probe(r.Context()); err != nil {
		logging.FromContext(r.Context()).Warn("readiness probe failed",
			logging.KeyComponent, o.component, logging.KeyError, err)
		return readyzBody{Status: "error", Backend: "error", Build: build, Error: "backend not ready"}, http.StatusServiceUnavailable
	}
	return readyzBody{Status: "ok", Backend: "ok", Build: build}, http.StatusOK
}

// observe wraps next with request metrics and structured logging. It installs a
// mutable request state on the context so inner routers can label the request
// with its format and operation.
func (o *obs) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := o.now()
		st := &requestState{}
		rr := &responseRecorder{ResponseWriter: w}
		next.ServeHTTP(rr, r.WithContext(contextWithState(r.Context(), st)))

		dur := o.now().Sub(start)
		status := rr.statusOrDefault()
		format := st.format
		if format == "" {
			format = "unknown"
		}
		op := opOrFallback(st.op, r.Method)
		bytesIn := requestBytesIn(r)

		o.recorder.HTTPRequest(format, op, strconv.Itoa(status), dur, bytesIn, rr.bytesOut)
		o.logRequest(r, status, dur, bytesIn, rr.bytesOut, format, op, st)
	})
}

func (o *obs) logRequest(r *http.Request, status int, dur time.Duration, bytesIn, bytesOut int64, format, op string, st *requestState) {
	attrs := []any{
		logging.KeyComponent, o.component,
		logging.KeyMethod, r.Method,
		logging.KeyPath, r.URL.Path,
		logging.KeyStatus, status,
		"duration_ms", dur.Milliseconds(),
		"bytes_in", bytesIn,
		"bytes_out", bytesOut,
		logging.KeyFormat, format,
		logging.KeyOp, op,
	}
	if st.namespace != "" {
		attrs = append(attrs, logging.KeyNamespace, st.namespace)
	}
	if rid := r.Header.Get("X-Request-Id"); rid != "" {
		attrs = append(attrs, "request_id", rid)
	}
	if st.err != nil {
		attrs = append(attrs, logging.KeyError, st.err.Error())
	}
	level := slog.LevelInfo
	if status >= http.StatusInternalServerError {
		level = slog.LevelError
	}
	logging.FromContext(r.Context()).Log(r.Context(), level, "http request", attrs...)
}

// opOrFallback returns the explicitly-set op, or a method-derived fallback.
func opOrFallback(op, method string) string {
	if op != "" {
		return op
	}
	switch method {
	case http.MethodGet, http.MethodHead:
		return "read"
	case http.MethodPut, http.MethodPost, http.MethodPatch:
		return "write"
	case http.MethodDelete:
		return "delete"
	default:
		return strings.ToLower(method)
	}
}

// requestBytesIn returns the known request body size, or 0 for unknown/chunked
// bodies (a negative ContentLength). It never reads the body.
func requestBytesIn(r *http.Request) int64 {
	if r.ContentLength > 0 {
		return r.ContentLength
	}
	return 0
}

// requestState carries the per-request labels inner routers populate.
type requestState struct {
	format    string
	op        string
	namespace string
	err       error
}

type ctxKey struct{}

func contextWithState(ctx context.Context, st *requestState) context.Context {
	return context.WithValue(ctx, ctxKey{}, st)
}

func stateFrom(ctx context.Context) *requestState {
	st, _ := ctx.Value(ctxKey{}).(*requestState)
	return st
}

// WrapWithFormat returns a middleware that labels every request flowing through
// it with format. It is applied where a format's routes are mounted so HTTP
// metrics and logs carry the right format label.
func WrapWithFormat(format string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if st := stateFrom(r.Context()); st != nil {
			st.format = format
		}
		next.ServeHTTP(w, r)
	})
}

// SetOperation labels the in-flight request with op. Routes call it when they
// know the operation; otherwise a method-based fallback applies.
func SetOperation(r *http.Request, op string) {
	if st := stateFrom(r.Context()); st != nil {
		st.op = op
	}
}

// SetNamespace records the namespace a route extracted, for the request log.
func SetNamespace(r *http.Request, namespace string) {
	if st := stateFrom(r.Context()); st != nil {
		st.namespace = namespace
	}
}

// RecordError records an error to surface in the request log. It does not
// change the response.
func RecordError(r *http.Request, err error) {
	if st := stateFrom(r.Context()); st != nil {
		st.err = err
	}
}

// responseRecorder wraps an http.ResponseWriter to capture the status code and
// response body size. A handler that writes a body without calling WriteHeader
// is recorded as 200, matching net/http's implicit behavior.
type responseRecorder struct {
	http.ResponseWriter
	status   int
	bytesOut int64
	wrote    bool
}

func (rr *responseRecorder) WriteHeader(code int) {
	if rr.wrote {
		return
	}
	rr.status = code
	rr.wrote = true
	rr.ResponseWriter.WriteHeader(code)
}

func (rr *responseRecorder) Write(b []byte) (int, error) {
	if !rr.wrote {
		rr.WriteHeader(http.StatusOK)
	}
	n, err := rr.ResponseWriter.Write(b)
	rr.bytesOut += int64(n)
	return n, err
}

func (rr *responseRecorder) statusOrDefault() int {
	if rr.status == 0 {
		return http.StatusOK
	}
	return rr.status
}

// Flush forwards to the underlying writer when it supports flushing, so
// streaming responses keep working through the wrapper.
func (rr *responseRecorder) Flush() {
	if f, ok := rr.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the underlying writer to http.ResponseController.
func (rr *responseRecorder) Unwrap() http.ResponseWriter { return rr.ResponseWriter }

// prober runs backend readiness probes with a timeout and a short success
// cache. A nil ping function makes probe always succeed (readiness collapses to
// liveness).
type prober struct {
	ping    Pinger
	timeout time.Duration
	ttl     time.Duration
	now     func() time.Time

	mu      sync.Mutex
	okUntil time.Time
}

func newProber(ping Pinger, now func() time.Time) *prober {
	return &prober{ping: ping, timeout: readyTimeout, ttl: readyCacheTTL, now: now}
}

func (p *prober) probe(ctx context.Context) error {
	if p.ping == nil {
		return nil
	}
	p.mu.Lock()
	cached := !p.okUntil.IsZero() && p.now().Before(p.okUntil)
	p.mu.Unlock()
	if cached {
		return nil
	}

	pctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	if err := p.ping(pctx); err != nil {
		return err
	}

	p.mu.Lock()
	p.okUntil = p.now().Add(p.ttl)
	p.mu.Unlock()
	return nil
}
