package command

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/spf13/cobra"
	"gocloud.dev/blob"

	"github.com/yolocs/open-artifact/pkg/auth"
	"github.com/yolocs/open-artifact/pkg/core/blobstore"
	"github.com/yolocs/open-artifact/pkg/logging"
	"github.com/yolocs/open-artifact/pkg/metrics"
	"github.com/yolocs/open-artifact/pkg/namespace"
	"github.com/yolocs/open-artifact/pkg/observability"
	"github.com/yolocs/open-artifact/pkg/surface/echo"
	"github.com/yolocs/open-artifact/pkg/surface/pypi"
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
	return serve(ctx, cfg, "serve", func(bkt *blob.Bucket, recorder metrics.Recorder) (planeHandler, error) {
		logger := logging.FromContext(ctx)
		authn := buildAuthenticator(cfg, logger)

		catalog, err := namespace.NewStore(bkt, cfg.BucketPrefix)
		if err != nil {
			return planeHandler{}, err
		}
		reg, err := namespace.NewRegistry(bkt, cfg.BucketPrefix, catalog, namespace.WithMetrics(recorder))
		if err != nil {
			return planeHandler{}, err
		}
		return planeHandler{
			handler: buildDataPlaneHandler(cfg, reg, authn, logger),
			pinger:  bucketPinger(bkt, cfg.BucketPrefix),
		}, nil
	})
}

// bucketPinger proves the data-plane bucket is reachable by probing for a
// sentinel object under the deployment root. A missing object is reachable
// (false, nil); only a backend/transport error is a readiness failure.
func bucketPinger(bkt *blob.Bucket, bucketPrefix string) observability.Pinger {
	key := blobstore.Root
	if bucketPrefix != "" {
		key += bucketPrefix + "/"
	}
	key += ".readyz-probe"
	return func(ctx context.Context) error {
		_, err := bkt.Exists(ctx, key)
		return err
	}
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
	if cfg.RepoType == "pypi" {
		mux.Handle("/", observability.WrapWithFormat("pypi", pypi.Handler(reg, authn, cfg.PyPI)))
	}
	return mux
}
