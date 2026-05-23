package command

import (
	"log/slog"

	"github.com/yolocs/open-artifact/pkg/auth"
	"github.com/yolocs/open-artifact/pkg/auth/chain"
	"github.com/yolocs/open-artifact/pkg/auth/oidc"
	"github.com/yolocs/open-artifact/pkg/logging"
)

// buildAuthenticator constructs the data-plane authenticator from the resolved
// config. With authn disabled it returns an always-anonymous authenticator and
// logs a warning; otherwise it builds an OIDC chain, one authenticator per
// configured issuer, all sharing the configured audience. Discovery is lazy, so
// this performs no network I/O.
func buildAuthenticator(cfg *runtimeConfig, logger *slog.Logger) auth.Authenticator {
	if cfg.DisableAuthn {
		logger.Warn("authentication is disabled; all requests are treated as anonymous",
			logging.KeyComponent, "auth")
		return auth.AlwaysAnonymous{}
	}
	children := make([]auth.Authenticator, 0, len(cfg.AuthnOIDCIssuers))
	for _, issuer := range cfg.AuthnOIDCIssuers {
		children = append(children, oidc.New(issuer, cfg.AuthnOIDCAudience))
	}
	return chain.New(children...)
}
