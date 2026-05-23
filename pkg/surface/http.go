package surface

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/yolocs/open-artifact/pkg/auth"
	"github.com/yolocs/open-artifact/pkg/core"
	"github.com/yolocs/open-artifact/pkg/namespace"
)

type NamespaceErrorContext int

const (
	NamespaceDataRead NamespaceErrorContext = iota
	NamespaceAdminWrite
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

func WriteStoreError(w http.ResponseWriter, r *http.Request, err error) {
	status, message, known := storeStatus(err)
	if !known {
		LoggerFromRequest(r).ErrorContext(r.Context(), "surface store error", slog.Any("error", err))
	}
	WriteError(w, status, message)
}

func WriteNamespaceError(w http.ResponseWriter, r *http.Request, err error, ctx NamespaceErrorContext) {
	status, message, known := namespaceStatus(err, ctx)
	if !known {
		LoggerFromRequest(r).ErrorContext(r.Context(), "surface namespace error", slog.Any("error", err))
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
	case errors.Is(err, namespace.ErrUnsupportedSchemaVersion):
		if ctx == NamespaceAdminWrite {
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

	rc, err := f.Read(r.Context())
	if err != nil {
		WriteStoreError(w, r, err)
		return
	}

	// core.File readers can surface ErrDigestMismatch at EOF. Buffering keeps
	// the helper able to return 422 before any response bytes are committed.
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, rc); err != nil {
		_ = rc.Close()
		WriteStoreError(w, r, err)
		return
	}
	if err := rc.Close(); err != nil {
		WriteStoreError(w, r, err)
		return
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

func ValidateName(name string) error {
	decoded, err := url.PathUnescape(name)
	if err != nil {
		return namespace.ErrInvalidName
	}
	if decoded == "" || strings.HasPrefix(decoded, "/") {
		return namespace.ErrInvalidName
	}
	for _, part := range strings.Split(decoded, "/") {
		if part == "" || part == "." || part == ".." || strings.HasPrefix(part, ".") {
			return namespace.ErrInvalidName
		}
	}
	return nil
}

func ExtractNamespace(r *http.Request, varName string) (string, error) {
	name := r.PathValue(varName)
	if err := ValidateName(name); err != nil {
		return "", err
	}
	return name, nil
}

func LoggerFromRequest(r *http.Request) *slog.Logger {
	if logger, ok := r.Context().Value(loggerKey{}).(*slog.Logger); ok && logger != nil {
		return logger
	}
	return slog.Default()
}

type loggerKey struct{}

func ContextWithLogger(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, logger)
}
