package debian

import (
	"net/http"
	"time"

	"github.com/yolocs/open-artifact/pkg/auth"
	"github.com/yolocs/open-artifact/pkg/core"
	"github.com/yolocs/open-artifact/pkg/namespace"
	"github.com/yolocs/open-artifact/pkg/observability"
	"github.com/yolocs/open-artifact/pkg/surface"
)

// DefaultProxyNegativeCacheTTL bounds how long an upstream 404 (index or
// artifact) is remembered in proxy mode.
const DefaultProxyNegativeCacheTTL = 30 * time.Second

// Config carries the Debian surface knobs. Debian is proxy-only, so there are
// no upload caps; a zero value uses the defaults.
type Config struct {
	// ProxyNegativeCacheTTL is how long an upstream 404 is remembered. A
	// non-positive value falls back to DefaultProxyNegativeCacheTTL.
	ProxyNegativeCacheTTL time.Duration
}

// Handler builds the Debian data-plane handler wrapped in the auth middleware.
func Handler(reg *namespace.Registry, authn auth.Authenticator, cfg Config) http.Handler {
	now := time.Now
	h := &handler{reg: reg, now: now, proxy: newProxyEngine(cfg, now)}
	return auth.Middleware(authn)(http.HandlerFunc(h.serveHTTP))
}

type handler struct {
	reg   *namespace.Registry
	now   func() time.Time
	proxy *proxyEngine
}

func (h *handler) serveHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		h.download(w, r)
	default:
		// Debian has no upload protocol: every mutation is rejected.
		surface.WriteMethodNotAllowed(w, []string{http.MethodGet, http.MethodHead})
	}
}

func (h *handler) download(w http.ResponseWriter, r *http.Request) {
	observability.SetOperation(r, "download")
	p, err := parsePath(r.URL.EscapedPath())
	if err != nil {
		surface.WriteStoreError(w, r, err)
		return
	}
	observability.SetNamespace(r, p.Namespace)

	store, spec, err := h.authorizedStore(w, r, p.Namespace)
	if err != nil {
		return
	}

	if spec.IsProxy() {
		pstore, ok := h.authorizedProxyStore(w, r, p.Namespace)
		if !ok {
			return
		}
		h.proxy.serve(w, r, p, p.Namespace, spec, pstore)
		return
	}

	// Hosted Debian namespaces have no upload protocol, but a bucket may be
	// pre-populated out of band; serve local content read-only, 404 otherwise.
	h.serveLocal(w, r, store, p)
}

// serveLocal serves a request from the local Store only (no upstream). It backs
// the hosted-mode read path.
func (h *handler) serveLocal(w http.ResponseWriter, r *http.Request, store core.Store, p requestPath) {
	switch p.Kind {
	case pathPool:
		f := store.Package(p.PoolDir).File(p.File)
		if exists, err := f.Exists(r.Context()); err != nil {
			surface.WriteStoreError(w, r, err)
			return
		} else if !exists {
			surface.WriteStoreError(w, r, core.ErrNotFound)
			return
		}
		surface.RedirectOrStreamFile(w, r, f, poolContentType(p.File))
	default:
		if !serveCachedIndex(w, r, store, p) {
			surface.WriteStoreError(w, r, core.ErrNotFound)
		}
	}
}

func (h *handler) authorizedStore(w http.ResponseWriter, r *http.Request, ns string) (core.Store, namespace.Spec, error) {
	ac := auth.FromContext(r.Context())
	store, spec, err := h.reg.AuthorizedStore(r.Context(), ns, debianFormat, ac)
	if err != nil {
		surface.WriteNamespaceError(w, r, err, surface.NamespaceDataRead)
		return nil, namespace.Spec{}, err
	}
	return store, spec, nil
}

// authorizedProxyStore authorizes the caller against the namespace reader
// policy and returns an unguarded store for pull-through cache fills.
func (h *handler) authorizedProxyStore(w http.ResponseWriter, r *http.Request, ns string) (core.Store, bool) {
	ac := auth.FromContext(r.Context())
	store, _, err := h.reg.AuthorizedProxyStore(r.Context(), ns, debianFormat, ac)
	if err != nil {
		surface.WriteNamespaceError(w, r, err, surface.NamespaceDataRead)
		return nil, false
	}
	return store, true
}
