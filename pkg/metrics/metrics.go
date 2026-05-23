// Package metrics is the observability recorder for open-artifact. It defines a
// small Recorder interface that the HTTP middleware and the blob backend report
// through, plus two implementations: a no-op (used when metrics are disabled)
// and a Prometheus-backed one. Keeping the surface a narrow interface lets the
// rest of the codebase stay free of a metrics-library dependency — pkg/core in
// particular instruments through a structurally-compatible local hook, never
// importing this package.
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// metricPrefix namespaces every metric this project exposes.
const metricPrefix = "open_artifact"

// Recorder records the project's runtime signals: per-request HTTP outcomes,
// blob-backend calls, and blob-redirect decisions. All methods must be safe for
// concurrent use and cheap enough to call on every request and backend op.
type Recorder interface {
	// HTTPRequest records one completed HTTP request. status is the numeric
	// status code rendered as a string; bytesIn/bytesOut are the request and
	// response body sizes (0 when unknown).
	HTTPRequest(format, op, status string, duration time.Duration, bytesIn, bytesOut int64)
	// BlobStoreCall records one backend blob operation and its classified
	// outcome.
	BlobStoreCall(op, status string, duration time.Duration)
	// BlobRedirect records the outcome of a download-URL decision: "redirected",
	// "inline", or "error".
	BlobRedirect(outcome string)
}

// NoOp returns a Recorder that discards everything. It is the recorder used
// when metrics are disabled so callers never need a nil check.
func NoOp() Recorder { return noop{} }

type noop struct{}

func (noop) HTTPRequest(string, string, string, time.Duration, int64, int64) {}
func (noop) BlobStoreCall(string, string, time.Duration)                     {}
func (noop) BlobRedirect(string)                                             {}

// Prometheus is a Recorder backed by a Prometheus registry. It also serves the
// exposition endpoint via Handler.
type Prometheus struct {
	reg *prometheus.Registry

	httpRequests *prometheus.CounterVec
	httpDuration *prometheus.HistogramVec
	httpBytesIn  *prometheus.CounterVec
	httpBytesOut *prometheus.CounterVec

	blobRequests *prometheus.CounterVec
	blobDuration *prometheus.HistogramVec
	blobRedirect *prometheus.CounterVec
}

// NewPrometheus builds a Prometheus recorder registered against reg. A nil reg
// creates a fresh registry seeded with the Go and process collectors so the
// exposition includes runtime metrics; a caller-supplied registry is used as-is
// so tests can isolate the project's own series.
func NewPrometheus(reg *prometheus.Registry) *Prometheus {
	if reg == nil {
		reg = prometheus.NewRegistry()
		reg.MustRegister(collectors.NewGoCollector())
		reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	}
	p := &Prometheus{
		reg: reg,
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: metricPrefix + "_http_requests_total",
			Help: "Total HTTP requests by format, op, and status.",
		}, []string{"format", "op", "status"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    metricPrefix + "_http_request_duration_seconds",
			Help:    "HTTP request duration in seconds by format and op.",
			Buckets: prometheus.DefBuckets,
		}, []string{"format", "op"}),
		httpBytesIn: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: metricPrefix + "_http_request_bytes_in_total",
			Help: "Total request body bytes by format and op.",
		}, []string{"format", "op"}),
		httpBytesOut: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: metricPrefix + "_http_response_bytes_out_total",
			Help: "Total response body bytes by format and op.",
		}, []string{"format", "op"}),
		blobRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: metricPrefix + "_blob_backend_requests_total",
			Help: "Total blob backend operations by op and status.",
		}, []string{"op", "status"}),
		blobDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    metricPrefix + "_blob_backend_request_duration_seconds",
			Help:    "Blob backend operation duration in seconds by op.",
			Buckets: prometheus.DefBuckets,
		}, []string{"op"}),
		blobRedirect: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: metricPrefix + "_blob_redirect_total",
			Help: "Total download-URL decisions by outcome.",
		}, []string{"outcome"}),
	}
	reg.MustRegister(
		p.httpRequests, p.httpDuration, p.httpBytesIn, p.httpBytesOut,
		p.blobRequests, p.blobDuration, p.blobRedirect,
	)
	return p
}

func (p *Prometheus) HTTPRequest(format, op, status string, duration time.Duration, bytesIn, bytesOut int64) {
	p.httpRequests.WithLabelValues(format, op, status).Inc()
	p.httpDuration.WithLabelValues(format, op).Observe(duration.Seconds())
	p.httpBytesIn.WithLabelValues(format, op).Add(float64(bytesIn))
	p.httpBytesOut.WithLabelValues(format, op).Add(float64(bytesOut))
}

func (p *Prometheus) BlobStoreCall(op, status string, duration time.Duration) {
	p.blobRequests.WithLabelValues(op, status).Inc()
	p.blobDuration.WithLabelValues(op).Observe(duration.Seconds())
}

func (p *Prometheus) BlobRedirect(outcome string) {
	p.blobRedirect.WithLabelValues(outcome).Inc()
}

// Handler returns the Prometheus exposition handler over this recorder's
// registry.
func (p *Prometheus) Handler() http.Handler {
	return promhttp.HandlerFor(p.reg, promhttp.HandlerOpts{})
}

// Registry exposes the underlying registry for tests and advanced wiring.
func (p *Prometheus) Registry() *prometheus.Registry { return p.reg }
