// Package logging provides the project's structured logger: a thin setup over
// log/slog plus context plumbing and a small set of stable field keys so logs
// are consistent across components. It is deliberately not a custom logging
// abstraction — callers use *slog.Logger directly.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// Stable field keys. Use these instead of ad-hoc strings so the same concept
// is queryable under one name everywhere.
const (
	KeyComponent = "component"
	KeyNamespace = "namespace"
	KeyFormat    = "format"
	KeyOp        = "op"
	KeyPath      = "path"
	KeyMethod    = "method"
	KeyStatus    = "status"
	KeyDuration  = "duration"
	KeyError     = "error"
)

// Options configures a logger. Zero values are not valid; use parsed config or
// NewFromEnv, which supply defaults.
type Options struct {
	// Level is one of debug, info, warn, error.
	Level string
	// Format is one of text, json.
	Format string
	// Debug, when true, includes caller/source location in records.
	Debug bool
}

// New builds a *slog.Logger writing to w according to opts. It returns an
// error for an unrecognized level or format so misconfiguration fails loudly
// at startup.
func New(w io.Writer, opts Options) (*slog.Logger, error) {
	level, err := parseLevel(opts.Level)
	if err != nil {
		return nil, err
	}
	hopts := &slog.HandlerOptions{Level: level, AddSource: opts.Debug}

	var h slog.Handler
	switch strings.ToLower(strings.TrimSpace(opts.Format)) {
	case "", "text":
		h = slog.NewTextHandler(w, hopts)
	case "json":
		h = slog.NewJSONHandler(w, hopts)
	default:
		return nil, fmt.Errorf("invalid log format %q: want text or json", opts.Format)
	}
	return slog.New(h), nil
}

// NewFromEnv builds a logger from <prefix>_LOG_LEVEL, <prefix>_LOG_FORMAT, and
// <prefix>_LOG_DEBUG, writing to stderr. Missing variables fall back to info /
// text / no-source.
func NewFromEnv(prefix string) (*slog.Logger, error) {
	get := func(suffix, def string) string {
		if v, ok := os.LookupEnv(prefix + suffix); ok {
			return v
		}
		return def
	}
	debug := false
	switch strings.ToLower(strings.TrimSpace(get("_LOG_DEBUG", "false"))) {
	case "1", "true", "yes":
		debug = true
	}
	return New(os.Stderr, Options{
		Level:  get("_LOG_LEVEL", "info"),
		Format: get("_LOG_FORMAT", "text"),
		Debug:  debug,
	})
}

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "", "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("invalid log level %q: want debug, info, warn, or error", s)
	}
}

type ctxKey struct{}

// ContextWithLogger returns a copy of ctx carrying logger.
func ContextWithLogger(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, logger)
}

// FromContext returns the logger stored in ctx, or slog.Default() if none.
func FromContext(ctx context.Context) *slog.Logger {
	if logger, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok && logger != nil {
		return logger
	}
	return slog.Default()
}
