package command

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/spf13/cobra"
	"gocloud.dev/blob"

	"github.com/yolocs/open-artifact/pkg/auth"
	"github.com/yolocs/open-artifact/pkg/logging"
	"github.com/yolocs/open-artifact/pkg/namespace"
	"github.com/yolocs/open-artifact/pkg/surface/echo"
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

// runServe is the real data-plane run function. It builds the authenticator and
// the namespace registry that authorizes per-namespace access; format routing
// (#25) installs the auth middleware and the authorized stores inside the
// format routes.
func runServe(ctx context.Context, cfg *runtimeConfig) error {
	return serve(ctx, cfg, "serve", func(bkt *blob.Bucket) (http.Handler, error) {
		logger := logging.FromContext(ctx)
		authn := buildAuthenticator(cfg, logger)

		catalog, err := namespace.NewStore(bkt, cfg.BucketPrefix)
		if err != nil {
			return nil, err
		}
		reg, err := namespace.NewRegistry(bkt, cfg.BucketPrefix, catalog)
		if err != nil {
			return nil, err
		}
		return buildDataPlaneHandler(cfg, reg, authn, logger), nil
	})
}

// buildDataPlaneHandler assembles the data-plane HTTP handler. The real package
// formats (#19-#25) will mount here, each guarding its routes with
// auth.Middleware(authn) and reaching storage through reg.Authorized so
// authentication and per-namespace authorization are enforced together. Until
// then the "echo" diagnostic surface exercises that same stack end to end.
func buildDataPlaneHandler(cfg *runtimeConfig, reg *namespace.Registry, authn auth.Authenticator, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	if cfg.RepoType == "echo" {
		mux.Handle("/", echo.Handler(reg, authn, logger))
	}
	return mux
}
