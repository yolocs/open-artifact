// Package integrationtest provides helpers for real-client and subprocess
// surface tests.
package integrationtest

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

type ServerSpec struct {
	Command    string
	Args       []string
	Env        map[string]string
	HealthPath string
	Timeout    time.Duration
}

type Server struct {
	cmd    *exec.Cmd
	URL    string
	cancel context.CancelFunc
	logs   safeBuffer
}

func (s *Server) Logs() string {
	return s.logs.String()
}

func (s *Server) Close() error {
	if s.cancel != nil {
		s.cancel()
	}
	if s.cmd == nil || s.cmd.Process == nil {
		return nil
	}
	err := s.cmd.Process.Kill()
	waitErr := s.cmd.Wait()
	if err != nil && !strings.Contains(err.Error(), "process already finished") {
		return err
	}
	if waitErr != nil && !strings.Contains(waitErr.Error(), "signal: killed") {
		return waitErr
	}
	return nil
}

func StartServer(ctx context.Context, spec ServerSpec) (*Server, error) {
	if spec.Command == "" {
		return nil, fmt.Errorf("integrationtest: empty server command")
	}
	if spec.HealthPath == "" {
		spec.HealthPath = "/healthz"
	}
	if spec.Timeout <= 0 {
		spec.Timeout = 10 * time.Second
	}

	addr, err := freeAddr()
	if err != nil {
		return nil, err
	}
	baseURL := "http://" + addr
	cmdCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(cmdCtx, spec.Command, spec.Args...)
	env := os.Environ()
	for k, v := range spec.Env {
		env = append(env, k+"="+strings.ReplaceAll(v, "{addr}", addr))
	}
	cmd.Env = env

	server := &Server{cmd: cmd, URL: baseURL, cancel: cancel}
	cmd.Stdout = &server.logs
	cmd.Stderr = &server.logs

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("integrationtest: start %s: %w", spec.Command, err)
	}
	if err := waitHealth(ctx, baseURL+spec.HealthPath, spec.Timeout, &server.logs); err != nil {
		_ = server.Close()
		return nil, err
	}
	return server, nil
}

func waitHealth(ctx context.Context, url string, timeout time.Duration, logs *safeBuffer) error {
	deadlineCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		req, err := http.NewRequestWithContext(deadlineCtx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
		}
		select {
		case <-deadlineCtx.Done():
			return fmt.Errorf("integrationtest: health check %s timed out: %w\nserver logs:\n%s", url, deadlineCtx.Err(), logs.String())
		case <-ticker.C:
		}
	}
}

type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func freeAddr() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("integrationtest: find free port: %w", err)
	}
	defer ln.Close()
	return ln.Addr().String(), nil
}

func MemBucketURL() string {
	return "mem://"
}

func FileBucketURL(dir string) string {
	return "file://" + filepath.ToSlash(dir)
}

type CommandRunner struct {
	Home     string
	TempDir  string
	ExtraEnv []string
}

type CommandRunnerOption func(*CommandRunner)

func WithCommandEnv(key, value string) CommandRunnerOption {
	return func(r *CommandRunner) {
		r.ExtraEnv = append(r.ExtraEnv, key+"="+value)
	}
}

func NewCommandRunner(root string, opts ...CommandRunnerOption) *CommandRunner {
	if root == "" {
		root = os.TempDir()
	}
	r := &CommandRunner{
		TempDir: root,
		Home:    filepath.Join(root, "home"),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

type CommandResult struct {
	Stdout string
	Stderr string
	Err    error
}

func (r *CommandRunner) Run(ctx context.Context, name string, args ...string) CommandResult {
	_ = os.MkdirAll(r.Home, 0o755)
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = r.Env()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return CommandResult{Stdout: stdout.String(), Stderr: stderr.String(), Err: err}
}

func (r *CommandRunner) Env() []string {
	env := append([]string{}, os.Environ()...)
	env = append(env,
		"HOME="+r.Home,
		"USERPROFILE="+r.Home,
		"XDG_CONFIG_HOME="+filepath.Join(r.TempDir, "config"),
		"XDG_CACHE_HOME="+filepath.Join(r.TempDir, "cache"),
	)
	if runtime.GOOS == "windows" {
		env = append(env, "APPDATA="+filepath.Join(r.TempDir, "AppData", "Roaming"))
	}
	env = append(env, r.ExtraEnv...)
	return env
}
