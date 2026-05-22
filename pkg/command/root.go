// Package command is the command layer for the open-artifact binary. It builds
// the cobra command tree (`serve` and `admin serve`), resolves flags and env
// vars through viper, and wires the runtime substrate — logger, bucket opener,
// and HTTP server lifecycle — together. It is the only layer that opens a
// concrete bucket and registers blob drivers.
package command

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"gocloud.dev/blob"

	"github.com/yolocs/open-artifact/internal/version"
	"github.com/yolocs/open-artifact/pkg/bucket"
	"github.com/yolocs/open-artifact/pkg/logging"
	"github.com/yolocs/open-artifact/pkg/metrics"
	"github.com/yolocs/open-artifact/pkg/observability"
	"github.com/yolocs/open-artifact/pkg/serving"
)

// runFunc is the seam between a resolved configuration and the work a command
// performs. The logger is carried on ctx (see pkg/logging). Tests substitute it
// to inspect resolution without starting a server.
type runFunc func(ctx context.Context, cfg *runtimeConfig) error

// Execute builds the root command and runs it, translating errors into a
// process exit code. SIGINT/SIGTERM cancel the command context, triggering
// graceful shutdown.
func Execute() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	root := NewRootCommand()
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "open-artifact: "+err.Error())
		return 1
	}
	return 0
}

// NewRootCommand assembles the full command tree wired to the real run
// functions.
func NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "open-artifact",
		Short:         "Stateless, multi-format artifact registry backed by a blob bucket",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version.String(),
	}
	root.SetVersionTemplate("{{.Version}}\n")

	root.AddCommand(newServeCommand(runServe))
	root.AddCommand(newAdminCommand())
	return root
}

// serveRunE returns a RunE that resolves config, builds the logger once, stores
// it on the context, and invokes run. It is shared by every serving command.
func serveRunE(run runFunc, dataPlane bool) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		cfg, err := resolveConfig(cmd, dataPlane)
		if err != nil {
			return err
		}
		logger, err := logging.New(cmd.ErrOrStderr(), logging.Options{
			Level:  cfg.LogLevel,
			Format: cfg.LogFormat,
			Debug:  cfg.LogDebug,
		})
		if err != nil {
			return err
		}
		ctx := logging.ContextWithLogger(cmd.Context(), logger)
		return run(ctx, cfg)
	}
}

// planeHandler is the plane-specific inner handler plus a backend readiness
// pinger, built from the open bucket and the shared metrics recorder.
type planeHandler struct {
	handler http.Handler
	pinger  observability.Pinger
}

// serve is the shared server lifecycle: open the bucket, build the metrics
// recorder, build the plane's handler, wrap it with observability, then serve
// until the context is cancelled. build receives the open bucket and the
// recorder (so backend calls are instrumented) and returns the plane handler
// and its readiness pinger; the logger is taken from ctx.
func serve(ctx context.Context, cfg *runtimeConfig, component string, build func(*blob.Bucket, metrics.Recorder) (planeHandler, error)) error {
	logger := logging.FromContext(ctx)

	bkt, cleanup, err := bucket.Open(ctx, cfg.BucketURL)
	if err != nil {
		return err
	}
	defer cleanup()

	logger.Info("starting server",
		logging.KeyComponent, component,
		"port", cfg.Port,
		"bucket_prefix", cfg.BucketPrefix,
		"metrics_enabled", cfg.EnableMetrics,
	)

	recorder, metricsHandler := buildRecorder(cfg)

	plane, err := build(bkt, recorder)
	if err != nil {
		return err
	}

	handler := observability.Wrap(observability.Config{
		Next:           plane.handler,
		Recorder:       recorder,
		MetricsHandler: metricsHandler,
		MetricsPath:    cfg.MetricsPath,
		Pinger:         plane.pinger,
		Component:      component,
	})

	addr := fmt.Sprintf(":%d", cfg.Port)
	return serving.Run(ctx, addr, handler)
}

// buildRecorder returns the metrics recorder and the exposition handler. When
// metrics are disabled it returns a no-op recorder and a nil handler, so the
// metrics endpoint is absent and no series are collected.
func buildRecorder(cfg *runtimeConfig) (metrics.Recorder, http.Handler) {
	if !cfg.EnableMetrics {
		return metrics.NoOp(), nil
	}
	prom := metrics.NewPrometheus(nil)
	return prom, prom.Handler()
}
