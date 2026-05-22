package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestNoOpDoesNothing(t *testing.T) {
	t.Parallel()
	r := NoOp()
	// Must not panic and must satisfy the interface.
	r.HTTPRequest("pypi", "read", "200", time.Second, 10, 20)
	r.BlobStoreCall("read_all", "ok", time.Second)
	r.BlobRedirect("redirected")
}

func TestPrometheusHTTPRequest(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	p := NewPrometheus(reg)

	p.HTTPRequest("pypi", "read", "200", 250*time.Millisecond, 100, 4096)
	p.HTTPRequest("pypi", "read", "200", 250*time.Millisecond, 0, 512)

	if got := counterValue(t, reg, "open_artifact_http_requests_total", map[string]string{
		"format": "pypi", "op": "read", "status": "200",
	}); got != 2 {
		t.Errorf("http_requests_total = %v, want 2", got)
	}
	if got := counterValue(t, reg, "open_artifact_http_request_bytes_in_total", map[string]string{
		"format": "pypi", "op": "read",
	}); got != 100 {
		t.Errorf("http_request_bytes_in_total = %v, want 100", got)
	}
	if got := counterValue(t, reg, "open_artifact_http_response_bytes_out_total", map[string]string{
		"format": "pypi", "op": "read",
	}); got != 4608 {
		t.Errorf("http_response_bytes_out_total = %v, want 4608", got)
	}
	if got := histogramCount(t, reg, "open_artifact_http_request_duration_seconds", map[string]string{
		"format": "pypi", "op": "read",
	}); got != 2 {
		t.Errorf("http_request_duration_seconds count = %v, want 2", got)
	}
}

func TestPrometheusBlobStoreCall(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	p := NewPrometheus(reg)

	p.BlobStoreCall("write_all", "ok", time.Millisecond)
	p.BlobStoreCall("exists", "not_found", time.Millisecond)
	p.BlobStoreCall("exists", "not_found", time.Millisecond)

	if got := counterValue(t, reg, "open_artifact_blob_backend_requests_total", map[string]string{
		"op": "exists", "status": "not_found",
	}); got != 2 {
		t.Errorf("blob_backend_requests_total{exists,not_found} = %v, want 2", got)
	}
	if got := counterValue(t, reg, "open_artifact_blob_backend_requests_total", map[string]string{
		"op": "write_all", "status": "ok",
	}); got != 1 {
		t.Errorf("blob_backend_requests_total{write_all,ok} = %v, want 1", got)
	}
	if got := histogramCount(t, reg, "open_artifact_blob_backend_request_duration_seconds", map[string]string{
		"op": "exists",
	}); got != 2 {
		t.Errorf("blob_backend_request_duration_seconds{exists} count = %v, want 2", got)
	}
}

func TestPrometheusBlobRedirect(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	p := NewPrometheus(reg)

	p.BlobRedirect("redirected")
	p.BlobRedirect("inline")
	p.BlobRedirect("inline")

	if got := counterValue(t, reg, "open_artifact_blob_redirect_total", map[string]string{"outcome": "inline"}); got != 2 {
		t.Errorf("blob_redirect_total{inline} = %v, want 2", got)
	}
	if got := counterValue(t, reg, "open_artifact_blob_redirect_total", map[string]string{"outcome": "redirected"}); got != 1 {
		t.Errorf("blob_redirect_total{redirected} = %v, want 1", got)
	}
}

func TestNewPrometheusNilRegistryIncludesRuntimeCollectors(t *testing.T) {
	t.Parallel()
	p := NewPrometheus(nil)
	mfs, err := p.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var hasGo bool
	for _, mf := range mfs {
		if mf.GetName() == "go_goroutines" {
			hasGo = true
		}
	}
	if !hasGo {
		t.Error("nil registry should seed the Go collector (go_goroutines missing)")
	}
}

// counterValue gathers reg and returns the value of the counter named name with
// exactly the given labels, or fails if absent.
func counterValue(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	m := findMetric(t, reg, name, labels)
	if m.Counter == nil {
		t.Fatalf("metric %q is not a counter", name)
	}
	return m.Counter.GetValue()
}

// histogramCount returns the sample count of the histogram named name with the
// given labels.
func histogramCount(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) uint64 {
	t.Helper()
	m := findMetric(t, reg, name, labels)
	if m.Histogram == nil {
		t.Fatalf("metric %q is not a histogram", name)
	}
	return m.Histogram.GetSampleCount()
}

func findMetric(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) *dto.Metric {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if labelsEqual(m, labels) {
				return m
			}
		}
	}
	t.Fatalf("metric %q with labels %v not found", name, labels)
	return nil
}

func labelsEqual(m *dto.Metric, want map[string]string) bool {
	got := make(map[string]string, len(m.GetLabel()))
	for _, lp := range m.GetLabel() {
		got[lp.GetName()] = lp.GetValue()
	}
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}
