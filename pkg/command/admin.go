package command

import (
	"context"

	"github.com/spf13/cobra"
)

// newAdminCommand builds the `admin` parent command and its subcommands.
func newAdminCommand() *cobra.Command {
	admin := &cobra.Command{
		Use:           "admin",
		Short:         "Control-plane commands",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	admin.AddCommand(newAdminServeCommand(runAdminServe))
	return admin
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

// runAdminServe is the real control-plane run function.
func runAdminServe(ctx context.Context, cfg *runtimeConfig) error {
	return serve(ctx, cfg, "admin")
}
