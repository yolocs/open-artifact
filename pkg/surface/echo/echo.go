// Package echo is a minimal diagnostic surface used to exercise the
// authentication and per-namespace authorization stack end to end before the
// real package-format surfaces (#19-#25) exist. It is not a package format: it
// performs a real namespace-scoped read or write and echoes the authenticated
// subject back. The data plane mounts it only when --repo-type=echo.
package echo

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/yolocs/open-artifact/pkg/auth"
	"github.com/yolocs/open-artifact/pkg/core"
	"github.com/yolocs/open-artifact/pkg/logging"
	"github.com/yolocs/open-artifact/pkg/namespace"
)

// Handler builds the echo surface. Every route is wrapped with the auth
// middleware (so missing/invalid credentials are 401), then authorizes a
// namespace-scoped operation: GET authorizes a read, PUT a write. Unknown
// namespaces map to 404 and denied operations to 403.
func Handler(reg *namespace.Registry, authn auth.Authenticator, logger *slog.Logger) http.Handler {
	h := &handler{reg: reg}
	mw := auth.Middleware(authn)
	mux := http.NewServeMux()
	mux.Handle("GET /{namespace}/echo", mw(http.HandlerFunc(h.read)))
	mux.Handle("PUT /{namespace}/echo", mw(http.HandlerFunc(h.write)))
	return mux
}

type handler struct {
	reg *namespace.Registry
}

// response is the echo body returned on an authorized request.
type response struct {
	Namespace string `json:"namespace"`
	Op        string `json:"op"`
	Issuer    string `json:"issuer"`
	ID        string `json:"id"`
	Email     string `json:"email,omitempty"`
	Kind      string `json:"kind"`
}

func (h *handler) read(w http.ResponseWriter, r *http.Request) {
	h.serve(w, r, false)
}

func (h *handler) write(w http.ResponseWriter, r *http.Request) {
	h.serve(w, r, true)
}

// serve resolves the authorized store and performs a read or write to drive the
// policy check, then echoes the subject.
func (h *handler) serve(w http.ResponseWriter, r *http.Request, write bool) {
	ctx := r.Context()
	ac := auth.FromContext(ctx)
	ns := r.PathValue("namespace")

	store, err := h.reg.Authorized(ctx, ns, string(core.FormatPyPI), ac)
	if err != nil {
		writeError(w, r, err)
		return
	}

	op := "read"
	if write {
		op = "write"
		// AddPackage authorizes a write before touching storage; on denial it
		// returns auth.ErrUnauthorized without creating anything.
		if _, err := store.AddPackage(ctx, "echo-probe"); err != nil && !errors.Is(err, core.ErrAlreadyExists) {
			writeError(w, r, err)
			return
		}
	} else if _, err := store.Packages(ctx); err != nil {
		writeError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, response{
		Namespace: ns,
		Op:        op,
		Issuer:    ac.Issuer,
		ID:        ac.ID,
		Email:     ac.Email,
		Kind:      ac.Kind,
	})
}

func writeError(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, namespace.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, auth.ErrUnauthorized):
		status = http.StatusForbidden
	case errors.Is(err, namespace.ErrInvalidName):
		status = http.StatusBadRequest
	}
	if status == http.StatusInternalServerError {
		logging.FromContext(r.Context()).Error("echo request failed",
			logging.KeyComponent, "echo", logging.KeyError, err)
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
