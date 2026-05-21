package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestNew(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		opts    Options
		wantErr bool
	}{
		{name: "defaults via empty strings", opts: Options{Level: "", Format: ""}},
		{name: "debug text", opts: Options{Level: "debug", Format: "text", Debug: true}},
		{name: "warn json", opts: Options{Level: "warn", Format: "json"}},
		{name: "uppercase accepted", opts: Options{Level: "ERROR", Format: "JSON"}},
		{name: "bad level", opts: Options{Level: "trace", Format: "text"}, wantErr: true},
		{name: "bad format", opts: Options{Level: "info", Format: "yaml"}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			logger, err := New(&buf, tc.opts)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("New(%+v) = nil error, want error", tc.opts)
				}
				return
			}
			if err != nil {
				t.Fatalf("New(%+v): %v", tc.opts, err)
			}
			if logger == nil {
				t.Fatal("New returned nil logger")
			}
		})
	}
}

func TestNewJSONEmitsStableFields(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger, err := New(&buf, Options{Level: "info", Format: "json"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	logger.Info("hello", KeyComponent, "serve", KeyStatus, 200)

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("unmarshal log line %q: %v", buf.String(), err)
	}
	if got := rec[KeyComponent]; got != "serve" {
		t.Errorf("component = %v, want serve", got)
	}
	if got := rec[KeyStatus]; got != float64(200) {
		t.Errorf("status = %v, want 200", got)
	}
}

func TestNewLevelFilters(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger, err := New(&buf, Options{Level: "warn", Format: "text"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	logger.Info("should be dropped")
	if buf.Len() != 0 {
		t.Errorf("info logged at warn level: %q", buf.String())
	}
	logger.Warn("kept")
	if !strings.Contains(buf.String(), "kept") {
		t.Errorf("warn not logged: %q", buf.String())
	}
}

func TestNewFromEnv(t *testing.T) {
	// Mutates process env; no t.Parallel per the env-var exception.
	t.Setenv("OA_LOG_LEVEL", "json-not-here")
	t.Setenv("OA_LOG_FORMAT", "json")
	t.Setenv("OA_LOG_LEVEL", "warn")
	t.Setenv("OA_LOG_DEBUG", "true")

	logger, err := NewFromEnv("OA")
	if err != nil {
		t.Fatalf("NewFromEnv: %v", err)
	}
	if logger == nil {
		t.Fatal("NewFromEnv returned nil logger")
	}

	t.Setenv("OA_LOG_FORMAT", "bogus")
	if _, err := NewFromEnv("OA"); err == nil {
		t.Fatal("NewFromEnv with bad format = nil error, want error")
	}
}

func TestContextRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	if got := FromContext(ctx); got != slog.Default() {
		t.Error("FromContext on empty context did not return slog.Default()")
	}

	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	ctx = ContextWithLogger(ctx, logger)
	if got := FromContext(ctx); got != logger {
		t.Error("FromContext did not return the stored logger")
	}
}
