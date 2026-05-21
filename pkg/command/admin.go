package command

import (
	"context"
	"net/http"

	"github.com/spf13/cobra"
	"gocloud.dev/blob"

	"github.com/yolocs/open-artifact/pkg/logging"
	"github.com/yolocs/open-artifact/pkg/namespace"
	"github.com/yolocs/open-artifact/pkg/surface/admin"
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

// runAdminServe is the real control-plane run function. It mounts the
// namespace CRUD API backed by the blob-bucket catalog.
func runAdminServe(ctx context.Context, cfg *runtimeConfig) error {
	return serve(ctx, cfg, "admin", func(bkt *blob.Bucket) (http.Handler, error) {
		store, err := namespace.NewStore(bkt, cfg.BucketPrefix)
		if err != nil {
			return nil, err
		}
		mux := http.NewServeMux()
		mux.Handle("/admin/v1/", admin.Handler(store, logging.FromContext(ctx)))
		return mux, nil
	})
}
