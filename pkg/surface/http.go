package surface

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/yolocs/open-artifact/pkg/auth"
	"github.com/yolocs/open-artifact/pkg/core"
	"github.com/yolocs/open-artifact/pkg/logging"
	"github.com/yolocs/open-artifact/pkg/namespace"
	"github.com/yolocs/open-artifact/pkg/observability"
)

type NamespaceErrorContext int

const (
	NamespaceDataRead NamespaceErrorContext = iota
	NamespaceDataWrite
)

func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func WriteError(w http.ResponseWriter, status int, message string) {
	WriteJSON(w, status, map[string]string{"error": message})
}

func WriteMethodNotAllowed(w http.ResponseWriter, allowed []string) {
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
}

func WithMaxBody(w http.ResponseWriter, r *http.Request, maxBytes int64) *http.Request {
	if maxBytes <= 0 {
		return r
	}
	next := r.Clone(r.Context())
	next.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	return next
}

// ReadCappedBody reads the request body fully, capped at maxBytes. It composes
// WithMaxBody so callers do not reimplement the cap or the over-limit detection.
// When the body exceeds the cap it returns tooLarge=true (the caller should
// respond 413); any other read failure is returned as err.
func ReadCappedBody(w http.ResponseWriter, r *http.Request, maxBytes int64) (body []byte, tooLarge bool, err error) {
	r = WithMaxBody(w, r, maxBytes)
	body, err = io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, true, nil
		}
		return nil, false, err
	}
	return body, false, nil
}

func WriteStoreError(w http.ResponseWriter, r *http.Request, err error) {
	status, message, known := storeStatus(err)
	if !known {
		observability.RecordError(r, err)
		logging.FromContext(r.Context()).Error("surface store error", logging.KeyComponent, "surface", logging.KeyError, err)
	}
	WriteError(w, status, message)
}

func WriteNamespaceError(w http.ResponseWriter, r *http.Request, err error, ctx NamespaceErrorContext) {
	status, message, known := namespaceStatus(err, ctx)
	if !known {
		observability.RecordError(r, err)
		logging.FromContext(r.Context()).Error("surface namespace error", logging.KeyComponent, "surface", logging.KeyError, err)
	}
	WriteError(w, status, message)
}

func storeStatus(err error) (int, string, bool) {
	switch {
	case errors.Is(err, core.ErrNotFound):
		return http.StatusNotFound, "not found", true
	case errors.Is(err, core.ErrAlreadyExists):
		return http.StatusConflict, "already exists", true
	case errors.Is(err, core.ErrDigestMismatch):
		return http.StatusUnprocessableEntity, "digest mismatch", true
	case errors.Is(err, core.ErrUnsupported):
		return http.StatusNotImplemented, "unsupported", true
	case errors.Is(err, core.ErrInvalidName):
		return http.StatusBadRequest, "invalid name", true
	case errors.Is(err, auth.ErrUnauthorized):
		return http.StatusForbidden, "forbidden", true
	default:
		return http.StatusInternalServerError, "internal server error", false
	}
}

func namespaceStatus(err error, ctx NamespaceErrorContext) (int, string, bool) {
	switch {
	case errors.Is(err, namespace.ErrInvalidName):
		return http.StatusBadRequest, "invalid namespace name", true
	case errors.Is(err, namespace.ErrInvalidProxy):
		return http.StatusBadRequest, "invalid proxy namespace", true
	case errors.Is(err, namespace.ErrInvalidPolicy):
		return http.StatusBadRequest, "invalid namespace policy", true
	case errors.Is(err, namespace.ErrUnsupportedSchemaVersion):
		if ctx == NamespaceDataWrite {
			return http.StatusBadRequest, "unsupported namespace schema version", true
		}
		return http.StatusInternalServerError, "internal server error", true
	case errors.Is(err, namespace.ErrNotFound):
		return http.StatusNotFound, "namespace not found", true
	case errors.Is(err, namespace.ErrNotEmpty):
		return http.StatusConflict, "namespace not empty", true
	case errors.Is(err, auth.ErrUnauthorized):
		return http.StatusForbidden, "forbidden", true
	default:
		return http.StatusInternalServerError, "internal server error", false
	}
}

func HeadAsGet(get http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			get.ServeHTTP(w, r)
			return
		}
		req := r.Clone(r.Context())
		req.Method = http.MethodGet
		get.ServeHTTP(&headResponseWriter{ResponseWriter: w}, req)
	})
}

type headResponseWriter struct {
	http.ResponseWriter
}

func (w *headResponseWriter) Write(p []byte) (int, error) {
	return len(p), nil
}

func RedirectOrStreamFile(w http.ResponseWriter, r *http.Request, f core.File, contentType string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		WriteMethodNotAllowed(w, []string{http.MethodGet, http.MethodHead})
		return
	}

	u, err := f.DownloadURL(r.Context())
	if err != nil {
		WriteStoreError(w, r, err)
		return
	}
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	if u != "" {
		w.Header().Set("Location", u)
		w.WriteHeader(http.StatusTemporaryRedirect)
		return
	}

	// No signed URL (e.g. mem/file backends): stream the blob straight to the
	// client with constant memory. The body is never buffered — integrity is
	// established at write time, object stores checksum at rest, and clients
	// verify downloads end-to-end — so there is no read-time re-hash to gate on.
	// Content-Length and ETag come from the recorded Meta (size + digest), so a
	// mid-stream backend failure after the header is sent simply truncates the
	// response, which the client detects via its own hash check.
	meta, err := f.Meta(r.Context())
	if err != nil {
		WriteStoreError(w, r, err)
		return
	}
	if meta.Size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
	}
	if meta.Digest != "" {
		w.Header().Set("ETag", strconv.Quote(meta.Digest))
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}

	rc, err := f.Read(r.Context())
	if err != nil {
		WriteStoreError(w, r, err)
		return
	}
	defer rc.Close()
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}
