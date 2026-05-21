// Package cli is the command layer for the open-artifact binary. It builds the
// cobra command tree (`serve` and `admin serve`), resolves flags and env vars
// through viper, and wires the runtime substrate — logger, bucket opener, and
// HTTP server lifecycle — together. It is the only layer that opens a concrete
// bucket and registers blob drivers.
package cli

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/yolocs/open-artifact/internal/bucket"
	"github.com/yolocs/open-artifact/internal/server"
	"github.com/yolocs/open-artifact/internal/version"
	"github.com/yolocs/open-artifact/pkg/logging"
)

// runFunc is the seam between a resolved configuration and the work a command
// performs. Tests substitute it to inspect resolution without starting a
// server.
type runFunc func(ctx context.Context, cfg *runtimeConfig, logger *slog.Logger) error

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

	admin := &cobra.Command{
		Use:           "admin",
		Short:         "Control-plane commands",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	admin.AddCommand(newAdminServeCommand(runAdminServe))

	root.AddCommand(newServeCommand(runServe))
	root.AddCommand(admin)
	return root
}

// newServeCommand builds the data-plane `serve` command.
func newServeCommand(run runFunc) *cobra.Command {
	cmd := &cobra.Command{
		Use:           "serve",
		Short:         "Run the data-plane server",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          serveRunE(run, true),
	}
	addSharedFlags(cmd.Flags(), defaultDataPort)
	addDataPlaneFlags(cmd.Flags())
	return cmd
}

// newAdminServeCommand builds the `admin serve` control-plane command.
func newAdminServeCommand(run runFunc) *cobra.Command {
	cmd := &cobra.Command{
		Use:           "serve",
		Short:         "Run the admin (control-plane) server",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          serveRunE(run, false),
	}
	addSharedFlags(cmd.Flags(), defaultAdminPort)
	return cmd
}

// serveRunE returns a RunE that resolves config, builds the logger, and invokes
// run.
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
		return run(cmd.Context(), cfg, logger)
	}
}

// runServe is the real data-plane run function: open the bucket, then serve
// until the context is cancelled. Format routing, namespaces, and auth are
// wired by later issues.
func runServe(ctx context.Context, cfg *runtimeConfig, logger *slog.Logger) error {
	return serve(ctx, cfg, logger, "serve")
}

// runAdminServe is the real control-plane run function.
func runAdminServe(ctx context.Context, cfg *runtimeConfig, logger *slog.Logger) error {
	return serve(ctx, cfg, logger, "admin")
}

func serve(ctx context.Context, cfg *runtimeConfig, logger *slog.Logger, component string) error {
	bkt, cleanup, err := bucket.Open(ctx, cfg.BucketURL)
	if err != nil {
		return err
	}
	defer cleanup()
	_ = bkt // The Store and surfaces consume the bucket in later issues.

	logger.Info("starting server",
		logging.KeyComponent, component,
		"port", cfg.Port,
		"bucket_prefix", cfg.BucketPrefix,
		"metrics_enabled", cfg.EnableMetrics,
	)

	mux := http.NewServeMux()
	addr := fmt.Sprintf(":%d", cfg.Port)
	return server.Run(ctx, addr, mux, logger)
}
