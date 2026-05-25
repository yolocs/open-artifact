package surface

import (
	"log/slog"
	"net/http"

	"github.com/yolocs/open-artifact/pkg/metrics"
)

type Middleware func(http.Handler) http.Handler

type Options struct {
	AuthMiddleware Middleware
	MaxUploadBytes int64
	Logger         *slog.Logger
	Metrics        metrics.Recorder
}

type Option func(*Options)

func NewOptions(opts ...Option) Options {
	out := Options{
		Logger:  slog.Default(),
		Metrics: metrics.NoOp(),
	}
	for _, opt := range opts {
		opt(&out)
	}
	if out.Logger == nil {
		out.Logger = slog.Default()
	}
	if out.Metrics == nil {
		out.Metrics = metrics.NoOp()
	}
	return out
}

func WithAuthMiddleware(mw Middleware) Option {
	return func(o *Options) {
		o.AuthMiddleware = mw
	}
}

func WithMaxUploadBytes(n int64) Option {
	return func(o *Options) {
		o.MaxUploadBytes = n
	}
}

func WithLogger(logger *slog.Logger) Option {
	return func(o *Options) {
		o.Logger = logger
	}
}

func WithMetrics(rec metrics.Recorder) Option {
	return func(o *Options) {
		o.Metrics = rec
	}
}
