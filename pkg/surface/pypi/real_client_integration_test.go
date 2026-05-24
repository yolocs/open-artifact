//go:build integration

package pypi_test

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/csv"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gocloud.dev/blob/memblob"

	"github.com/yolocs/open-artifact/pkg/surface/pypi"
)

func TestRealPipDownloadAndInstall(t *testing.T) {
	t.Parallel()

	python := requirePython(t)
	h := newHarness(t, memblob.OpenBucket(nil), pypi.Config{SimpleIndexCacheTTL: 0})
	wheel := buildWheel(t, "demo-pkg", "demo_pkg", "0.1.0")
	resp := upload(t, h, "team-a", "demo-pkg", "0.1.0", filepath.Base(wheel), mustRead(t, wheel), nil)
	if resp.StatusCode != 201 {
		t.Fatalf("upload status = %d: %s", resp.StatusCode, readResp(t, resp))
	}
	_ = readResp(t, resp)

	venv := filepath.Join(t.TempDir(), "venv")
	runCmd(t, "", python, "-m", "venv", venv)
	venvPython := venvPython(t, venv)
	pipEnv := append(os.Environ(),
		"PIP_DISABLE_PIP_VERSION_CHECK=1",
		"PIP_NO_INPUT=1",
		"PYTHONNOUSERSITE=1",
	)
	indexURL := h.server.URL + "/team-a/simple/"
	downloadDir := filepath.Join(t.TempDir(), "download")
	if err := os.MkdirAll(downloadDir, 0o755); err != nil {
		t.Fatalf("MkdirAll download: %v", err)
	}
	runCmdEnv(t, pipEnv, "", venvPython, "-m", "pip", "download",
		"--no-deps",
		"--index-url", indexURL,
		"--trusted-host", hostOnly(t, indexURL),
		"--dest", downloadDir,
		"demo-pkg==0.1.0",
	)
	if _, err := os.Stat(filepath.Join(downloadDir, filepath.Base(wheel))); err != nil {
		t.Fatalf("pip download did not write wheel: %v", err)
	}
	runCmdEnv(t, pipEnv, "", venvPython, "-m", "pip", "install",
		"--no-deps",
		"--index-url", indexURL,
		"--trusted-host", hostOnly(t, indexURL),
		"demo-pkg==0.1.0",
	)
	runCmdEnv(t, pipEnv, "", venvPython, "-c", `import demo_pkg; assert demo_pkg.hello() == "ok"`)
}

func TestRealTwineUpload(t *testing.T) {
	t.Parallel()

	python := requirePython(t)
	if err := exec.Command(python, "-m", "twine", "--version").Run(); err != nil {
		t.Skipf("python -m twine is not available: %v", err)
	}
	h := newHarness(t, memblob.OpenBucket(nil), pypi.Config{SimpleIndexCacheTTL: 0})
	wheel := buildWheel(t, "twine-demo", "twine_demo", "0.1.0")
	env := append(os.Environ(), "TWINE_NON_INTERACTIVE=1")
	repositoryURL := h.server.URL + "/team-a/"
	runCmdEnv(t, env, "", python, "-m", "twine", "upload",
		"--repository-url", repositoryURL,
		"--username", "anonymous",
		"--password", "anonymous",
		wheel,
	)
	project := readResp(t, get(t, h, "/team-a/simple/twine-demo/", ""))
	if !bytes.Contains([]byte(project), []byte(filepath.Base(wheel))) {
		t.Fatalf("twine upload missing from simple index: %s", project)
	}
}

func requirePython(t *testing.T) string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skipf("python3 is not available: %v", err)
	}
	runCmd(t, "", python, "-m", "pip", "--version")
	return python
}

func buildWheel(t *testing.T, distName, moduleName, version string) string {
	t.Helper()
	wheelName := strings.ReplaceAll(distName, "-", "_")
	filename := fmt.Sprintf("%s-%s-py3-none-any.whl", wheelName, version)
	path := filepath.Join(t.TempDir(), filename)
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("Create wheel: %v", err)
	}
	defer f.Close()

	zw := zip.NewWriter(f)
	records := map[string][]byte{
		moduleName + "/__init__.py": []byte(`def hello():
    return "ok"
`),
		fmt.Sprintf("%s-%s.dist-info/METADATA", wheelName, version): []byte(fmt.Sprintf("Metadata-Version: 2.1\nName: %s\nVersion: %s\nSummary: open-artifact integration package\n", distName, version)),
		fmt.Sprintf("%s-%s.dist-info/WHEEL", wheelName, version):    []byte("Wheel-Version: 1.0\nGenerator: open-artifact integration test\nRoot-Is-Purelib: true\nTag: py3-none-any\n"),
	}
	recordPath := fmt.Sprintf("%s-%s.dist-info/RECORD", wheelName, version)
	var recordRows [][]string
	for name, body := range records {
		if err := writeZipFile(zw, name, body); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		sum := sha256.Sum256(body)
		recordRows = append(recordRows, []string{
			name,
			"sha256=" + base64.RawURLEncoding.EncodeToString(sum[:]),
			fmt.Sprint(len(body)),
		})
	}
	recordRows = append(recordRows, []string{recordPath, "", ""})
	var record bytes.Buffer
	cw := csv.NewWriter(&record)
	if err := cw.WriteAll(recordRows); err != nil {
		t.Fatalf("write RECORD csv: %v", err)
	}
	if err := writeZipFile(zw, recordPath, record.Bytes()); err != nil {
		t.Fatalf("write RECORD: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("Close zip: %v", err)
	}
	return path
}

func writeZipFile(zw *zip.Writer, name string, body []byte) error {
	w, err := zw.Create(name)
	if err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return b
}

func venvPython(t *testing.T, venv string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		return filepath.Join(venv, "Scripts", "python.exe")
	}
	return filepath.Join(venv, "bin", "python")
}

func hostOnly(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("Parse index URL: %v", err)
	}
	return u.Hostname()
}

func runCmd(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	runCmdEnv(t, os.Environ(), dir, name, args...)
}

func runCmdEnv(t *testing.T, env []string, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), name, args...)
	cmd.Dir = dir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %v: %v\nstdout:\n%s\nstderr:\n%s", name, args, err, stdout.String(), stderr.String())
	}
}
