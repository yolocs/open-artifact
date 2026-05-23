package integrationtest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func LocateBinary(envVar, fallback string) (string, bool) {
	if envVar != "" {
		if path := os.Getenv(envVar); path != "" {
			if executable(path) {
				return path, true
			}
			return "", false
		}
	}
	if fallback != "" && executable(fallback) {
		return fallback, true
	}
	return "", false
}

func BuildOpenArtifact(ctx context.Context, outDir string) (string, error) {
	if outDir == "" {
		return "", fmt.Errorf("integrationtest: empty output directory")
	}
	root, err := repoRoot()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", fmt.Errorf("integrationtest: create output directory: %w", err)
	}
	out := filepath.Join(outDir, "open-artifact")
	cmd := exec.CommandContext(ctx, "go", "build", "-o", out, "./cmd/open-artifact")
	cmd.Dir = root
	raw, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("integrationtest: build open-artifact: %w\n%s", err, raw)
	}
	return out, nil
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("integrationtest: could not find go.mod")
		}
		dir = parent
	}
}

func executable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode()&0o111 != 0
}
