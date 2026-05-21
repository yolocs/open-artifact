package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestServeGracefulShutdown(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "pong")
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, ln, mux, nil) }()

	resp, err := http.Get("http://" + addr + "/ping")
	if err != nil {
		t.Fatalf("GET /ping: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if got := string(body); got != "pong" {
		t.Errorf("body = %q, want pong", got)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v, want nil after graceful shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after context cancellation")
	}

	if _, err := http.Get("http://" + addr + "/ping"); err == nil {
		t.Error("server still accepting connections after shutdown")
	}
}

func TestRunDynamicPort(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, "127.0.0.1:0", http.NewServeMux(), nil) }()

	// Give Run a moment to bind, then cancel; it should shut down cleanly.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

func TestRunListenError(t *testing.T) {
	t.Parallel()

	if err := Run(t.Context(), "127.0.0.1:-1", http.NewServeMux(), nil); err == nil {
		t.Fatal("Run with invalid addr = nil error, want error")
	}
}
