// Package metrics defines observability contracts used by HTTP surfaces and
// backend instrumentation.
package metrics

import "time"

type Recorder interface {
	HTTPRequest(format, op, status string, duration time.Duration, bytesIn, bytesOut int64)
	BlobStoreCall(op, status string, duration time.Duration)
	BlobRedirect(outcome string)
}

type noopRecorder struct{}

func NoOp() Recorder { return noopRecorder{} }

func (noopRecorder) HTTPRequest(string, string, string, time.Duration, int64, int64) {}
func (noopRecorder) BlobStoreCall(string, string, time.Duration)                     {}
func (noopRecorder) BlobRedirect(string)                                             {}
