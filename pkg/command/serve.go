package command

import (
	"context"

	"github.com/spf13/cobra"
)

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

// runServe is the real data-plane run function.
func runServe(ctx context.Context, cfg *runtimeConfig) error {
	return serve(ctx, cfg, "serve")
}
