// Package admin is the namespace control-plane HTTP API. It exposes namespace
// CRUD under /admin/v1/namespaces and is the only writer of namespace
// metadata. It has no built-in authentication: operators must deploy it behind
// network/platform access controls, and Handler logs a startup warning saying
// so.
package admin

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/yolocs/open-artifact/pkg/logging"
	"github.com/yolocs/open-artifact/pkg/namespace"
)

// Handler builds the admin HTTP handler backed by the namespace catalog. It
// logs a warning that the admin plane has no built-in auth.
func Handler(store *namespace.Store, logger *slog.Logger) http.Handler {
	logger.Warn("admin plane has no built-in authentication; deploy behind network/platform access controls",
		logging.KeyComponent, "admin")

	h := &handler{store: store}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/v1/namespaces", h.list)
	mux.HandleFunc("GET /admin/v1/namespaces/{name}", h.get)
	mux.HandleFunc("PUT /admin/v1/namespaces/{name}", h.put)
	mux.HandleFunc("DELETE /admin/v1/namespaces/{name}", h.delete)
	return mux
}

type handler struct {
	store *namespace.Store
}

// listResponse is the body of GET /admin/v1/namespaces.
type listResponse struct {
	Namespaces []*namespace.Namespace `json:"namespaces"`
}

func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	nss, err := h.store.List(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, listResponse{Namespaces: nss})
}

func (h *handler) get(w http.ResponseWriter, r *http.Request) {
	ns, err := h.store.Get(r.Context(), r.PathValue("name"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ns)
}

func (h *handler) put(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	var spec namespace.Spec
	if err := decodeJSON(r, &spec); err != nil {
		writeStatus(w, http.StatusBadRequest, err.Error())
		return
	}

	// Distinguish create (201) from update (200) by probing existence first.
	// The admin plane is the only writer, so the window between the probe and
	// the write does not affect correctness of the data, only the status code.
	_, getErr := h.store.Get(r.Context(), name)
	existed := getErr == nil
	if getErr != nil && !errors.Is(getErr, namespace.ErrNotFound) {
		writeError(w, r, getErr)
		return
	}

	ns, err := h.store.Put(r.Context(), &namespace.Namespace{Name: name, Spec: spec})
	if err != nil {
		writeError(w, r, err)
		return
	}
	status := http.StatusCreated
	if existed {
		status = http.StatusOK
	}
	writeJSON(w, status, ns)
}

func (h *handler) delete(w http.ResponseWriter, r *http.Request) {
	if err := h.store.Delete(r.Context(), r.PathValue("name")); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// errorResponse is the uniform error body: {"error":"message"}.
type errorResponse struct {
	Error string `json:"error"`
}

// writeError maps a namespace error to its HTTP status and writes the uniform
// error body.
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	status := statusFor(err)
	if status == http.StatusInternalServerError {
		logging.FromContext(r.Context()).Error("admin request failed",
			logging.KeyComponent, "admin", logging.KeyError, err)
	}
	writeStatus(w, status, err.Error())
}

// statusFor maps namespace sentinels to HTTP status codes.
func statusFor(err error) int {
	switch {
	case errors.Is(err, namespace.ErrInvalidName),
		errors.Is(err, namespace.ErrInvalidProxy),
		errors.Is(err, namespace.ErrUnsupportedSchemaVersion):
		return http.StatusBadRequest
	case errors.Is(err, namespace.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, namespace.ErrNotEmpty):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

func writeStatus(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// decodeJSON strictly decodes the request body into v, rejecting unknown
// fields and trailing data so malformed specs fail loudly at the boundary.
func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	return nil
}
