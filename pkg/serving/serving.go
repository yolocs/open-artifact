// Package serving is the shared HTTP server lifecycle helper used by both the
// data plane and the admin plane. It serves a handler until the supplied
// context is cancelled, then drains in-flight requests with a bounded
// graceful-shutdown window. The logger is taken from the context (see
// pkg/logging) and injected onto every request via WithLogger.
package serving

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/yolocs/open-artifact/pkg/logging"
)

// shutdownTimeout bounds how long Serve waits for in-flight requests to drain
// after the context is cancelled.
const shutdownTimeout = 15 * time.Second

// WithLogger returns a middleware that places logger on each request's context
// so downstream handlers retrieve it with logging.FromContext.
func WithLogger(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := logging.ContextWithLogger(r.Context(), logger)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Run listens on addr and serves handler until ctx is cancelled. addr follows
// net.Listen("tcp", ...) semantics; use "127.0.0.1:0" or ":0" for a dynamic
// port. To observe the chosen port, create the listener yourself and call
// Serve.
func Run(ctx context.Context, addr string, handler http.Handler) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %q: %w", addr, err)
	}
	return Serve(ctx, ln, handler)
}

// Serve serves handler on ln until ctx is cancelled, then shuts down
// gracefully. The logger comes from ctx (see pkg/logging) and is injected onto
// every request. It always closes ln. A clean shutdown returns nil.
func Serve(ctx context.Context, ln net.Listener, handler http.Handler) error {
	logger := logging.FromContext(ctx)
	srv := &http.Server{
		Handler:     WithLogger(logger, handler),
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	idleClosed := make(chan struct{})
	go func() {
		<-ctx.Done()
		logger.Info("shutting down http server", logging.KeyComponent, "serving")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown failed", logging.KeyComponent, "serving", logging.KeyError, err)
			_ = srv.Close()
		}
		close(idleClosed)
	}()

	logger.Info("http server listening", logging.KeyComponent, "serving", logging.KeyPath, ln.Addr().String())
	err := srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		<-idleClosed
		return nil
	}
	return err
}
