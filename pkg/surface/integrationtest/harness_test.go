package integrationtest

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/yolocs/open-artifact/pkg/auth"
	"github.com/yolocs/open-artifact/pkg/namespace"
)

func TestLocalBucketURLs(t *testing.T) {
	t.Parallel()

	fileDir := t.TempDir()
	tests := []struct {
		name string
		got  string
		want string
	}{
		{name: "mem", got: MemBucketURL(), want: "mem://"},
		{name: "file", got: FileBucketURL(fileDir), want: "file://" + fileDir},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.got != tt.want {
				t.Fatalf("bucket URL mismatch (-want +got):\n%s", cmp.Diff(tt.want, tt.got))
			}
		})
	}
}

func TestLocateBinaryUsesFallback(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	bin := filepath.Join(dir, "open-artifact")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, ok := LocateBinary("", bin)
	if !ok {
		t.Fatal("LocateBinary ok = false, want true")
	}
	if got != bin {
		t.Fatalf("binary = %q, want %q", got, bin)
	}
}

func TestBuildOpenArtifactBuildsServerBinary(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	bin, err := BuildOpenArtifact(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("BuildOpenArtifact: %v", err)
	}
	if _, ok := LocateBinary("", bin); !ok {
		t.Fatalf("built binary %q is not executable", bin)
	}
}

func TestCommandRunnerUsesIsolatedHomeAndCapturesOutput(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	r := NewCommandRunner(t.TempDir(), WithCommandEnv("GO_WANT_HELPER_COMMAND", "1"))
	result := r.Run(ctx, os.Args[0], "-test.run=TestHelperCommand", "--", "ignored")

	if result.Err != nil {
		t.Fatalf("Run: %v\nstdout=%s\nstderr=%s", result.Err, result.Stdout, result.Stderr)
	}
	if got := strings.TrimSpace(result.Stdout); got != r.Home {
		t.Fatalf("stdout HOME = %q, want %q", got, r.Home)
	}
}

func TestServerHarnessStartsWaitsForHealthzAndTerminates(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	server, err := StartServer(ctx, ServerSpec{
		Command: os.Args[0],
		Args:    []string{"-test.run=TestHelperServer"},
		Env: map[string]string{
			"GO_WANT_HELPER_SERVER": "1",
			"OA_TEST_ADDR":          "{addr}",
		},
		HealthPath: "/healthz",
		Timeout:    5 * time.Second,
	})
	if err != nil {
		t.Fatalf("StartServer: %v", err)
	}
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	resp, err := http.Get(server.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d\nlogs=%s", resp.StatusCode, http.StatusOK, server.Logs())
	}
}

func TestNamespaceHelpersCreateExpectedPolicies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		spec namespace.Spec
		want namespace.Spec
	}{
		{
			name: "hosted anonymous reader writer",
			spec: HostedAnonymous("pypi/global"),
			want: namespace.Spec{
				Name: "pypi/global",
				Kind: namespace.KindHosted,
				Policy: namespace.Policy{
					Readers: []namespace.SubjectMatcher{{Issuer: auth.AnonymousIssuer, ID: auth.AnonymousID, Kind: auth.KindAnonymous}},
					Writers: []namespace.SubjectMatcher{{Issuer: auth.AnonymousIssuer, ID: auth.AnonymousID, Kind: auth.KindAnonymous}},
				},
			},
		},
		{
			name: "proxy anonymous reader",
			spec: ProxyAnonymous("npm/global", "https://registry.npmjs.org"),
			want: namespace.Spec{
				Name:        "npm/global",
				Kind:        namespace.KindProxy,
				UpstreamURL: "https://registry.npmjs.org",
				Policy: namespace.Policy{
					Readers: []namespace.SubjectMatcher{{Issuer: auth.AnonymousIssuer, ID: auth.AnonymousID, Kind: auth.KindAnonymous}},
				},
			},
		},
		{
			name: "deny all",
			spec: DenyAll("maven/global"),
			want: namespace.Spec{Name: "maven/global", Kind: namespace.KindHosted},
		},
		{
			name: "read only",
			spec: ReadOnlyAnonymous("pypi/readonly"),
			want: namespace.Spec{
				Name: "pypi/readonly",
				Kind: namespace.KindHosted,
				Policy: namespace.Policy{
					Readers: []namespace.SubjectMatcher{{Issuer: auth.AnonymousIssuer, ID: auth.AnonymousID, Kind: auth.KindAnonymous}},
				},
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(tt.want, tt.spec); diff != "" {
				t.Fatalf("namespace helper mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestSeedNamespaceCreatesRegistrySpec(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	registry := namespace.NewRegistry()
	spec := HostedAnonymous("pypi/global")

	if err := SeedNamespace(ctx, registry, spec, nil); err != nil {
		t.Fatalf("SeedNamespace: %v", err)
	}
	got, err := registry.Spec(ctx, "pypi/global")
	if err != nil {
		t.Fatalf("Spec: %v", err)
	}
	if diff := cmp.Diff(spec, got); diff != "" {
		t.Fatalf("seeded namespace mismatch (-want +got):\n%s", diff)
	}
}

func TestHelperCommand(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_COMMAND") != "1" {
		return
	}
	_, _ = os.Stdout.WriteString(os.Getenv("HOME") + "\n")
	os.Exit(0)
}

func TestHelperServer(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_SERVER") != "1" {
		return
	}
	addr := os.Getenv("OA_TEST_ADDR")
	if addr == "" {
		os.Exit(2)
	}
	srv := &http.Server{
		Addr: addr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/healthz" {
				_, _ = w.Write([]byte("ok\n"))
				return
			}
			http.NotFound(w, r)
		}),
	}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		os.Exit(3)
	}
}

func TestServerHarnessIncludesLogsOnStartupFailure(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	_, err := StartServer(ctx, ServerSpec{
		Command: os.Args[0],
		Args:    []string{"-test.run=TestHelperFailingServer"},
		Env: map[string]string{
			"GO_WANT_HELPER_FAILING_SERVER": "1",
			"OA_TEST_ADDR":                  "{addr}",
		},
		HealthPath: "/healthz",
		Timeout:    250 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("StartServer succeeded, want failure")
	}
	if !strings.Contains(err.Error(), "intentional startup failure") {
		t.Fatalf("error %q does not include server logs", err)
	}
}

func TestHelperFailingServer(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_FAILING_SERVER") != "1" {
		return
	}
	_, _ = os.Stderr.WriteString("intentional startup failure\n")
	os.Exit(4)
}

func TestCommandRunnerSetsHelperEnv(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	r := NewCommandRunner(t.TempDir())
	result := r.Run(ctx, os.Args[0], "-test.run=TestHelperCommand")
	if result.Err != nil {
		t.Fatalf("Run without helper env failed unexpectedly: %v", result.Err)
	}

	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestHelperCommand")
	cmd.Env = r.Env()
	cmd.Env = append(cmd.Env, "GO_WANT_HELPER_COMMAND=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("manual helper command: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != r.Home {
		t.Fatalf("stdout HOME = %q, want %q", got, r.Home)
	}
}
