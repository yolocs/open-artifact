package surface

import (
	"context"
	"net/http"
	"strings"
)

type labelState struct {
	format string
	op     string
}

type labelKey struct{}

func WrapWithFormat(format string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			state := &labelState{format: format, op: fallbackOperation(r.Method)}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), labelKey{}, state)))
		})
	}
}

func SetOperation(r *http.Request, op string) {
	if state, ok := r.Context().Value(labelKey{}).(*labelState); ok {
		state.op = op
	}
}

func Format(r *http.Request) string {
	state, ok := r.Context().Value(labelKey{}).(*labelState)
	if !ok || state.format == "" {
		return "unknown"
	}
	return state.format
}

func Operation(r *http.Request) string {
	state, ok := r.Context().Value(labelKey{}).(*labelState)
	if !ok || state.op == "" {
		return fallbackOperation(r.Method)
	}
	return state.op
}

func fallbackOperation(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead:
		return "read"
	case http.MethodPut, http.MethodPost, http.MethodPatch:
		return "write"
	case http.MethodDelete:
		return "delete"
	default:
		return strings.ToLower(method)
	}
}
