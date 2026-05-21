package surface

import (
	"errors"
	"net/http"

	"github.com/yolocs/open-artifact/pkg/core"
)

// Handler is a mounted protocol surface. A surface registers its routes on a
// shared mux under prefix; the prefix is the URL segment the operator chose
// for the format (for example "/pypi"). Mount is the only contract a surface
// exposes to the server binary.
type Handler interface {
	// Mount registers the surface's routes on mux under prefix. A trailing
	// slash on prefix is insignificant; an empty prefix mounts at the root.
	Mount(prefix string, mux *http.ServeMux)
}

// WriteStoreError maps a core.Store sentinel error to its HTTP response and
// writes it. It returns true when err is non-nil (and thus an error response
// was written), false when err is nil and the caller should proceed.
//
// The mapping is the one shared by every surface:
//
//	core.ErrNotFound       → 404 Not Found
//	core.ErrAlreadyExists  → 409 Conflict
//	core.ErrDigestMismatch → 422 Unprocessable Entity
//	core.ErrUnsupported    → 501 Not Implemented
//	anything else          → 500 Internal Server Error
func WriteStoreError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	status, msg := classifyStoreError(err)
	http.Error(w, msg, status)
	return true
}

// classifyStoreError returns the HTTP status and a short, client-safe message
// for a Store error. Internal error detail is never echoed back to the client
// — only the generic status text — so backend wording can't leak through the
// surface.
func classifyStoreError(err error) (int, string) {
	switch {
	case errors.Is(err, core.ErrNotFound):
		return http.StatusNotFound, "not found"
	case errors.Is(err, core.ErrAlreadyExists):
		return http.StatusConflict, "already exists"
	case errors.Is(err, core.ErrDigestMismatch):
		return http.StatusUnprocessableEntity, "digest mismatch"
	case errors.Is(err, core.ErrUnsupported):
		return http.StatusNotImplemented, "unsupported"
	default:
		return http.StatusInternalServerError, "internal error"
	}
}
