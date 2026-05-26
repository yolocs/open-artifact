//go:build integration

package debian_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gocloud.dev/blob/memblob"

	"github.com/yolocs/open-artifact/pkg/surface/debian"
)

// TestDebianProxyAptGetDownload drives the real `apt-get` client through a
// proxy-mode namespace whose upstream is an in-process fake APT repository. It
// proves the pull-through path end to end with the actual Debian client:
// `apt-get update` fetches the (cached) Release/Packages indexes through the
// proxy, `apt-get download` fetches the .deb (filling the blob cache), and a
// subsequent direct GET to open-artifact serves the cached .deb with upstream
// down.
//
// The repository is served over plain HTTP with sources.list `[trusted=yes]`,
// so no GPG signing is needed; apt still verifies the Packages index against
// the Release checksums and the .deb against the Packages checksum, which the
// verbatim proxy passthrough preserves. All apt state lives in t.TempDir()
// directories, so the test needs neither root nor Docker.
func TestDebianProxyAptGetDownload(t *testing.T) {
	t.Parallel()

	aptGet := requireApt(t)
	arch := dpkgArch(t)

	repo := newAptRepo(t, arch)
	up := newFakeDebian(t)
	for path, body := range repo.files {
		up.put(path, body)
	}

	h := newProxyHarness(t, memblob.OpenBucket(nil), debian.Config{}, proxyNS("team-proxy", up.server.URL))

	root := t.TempDir()
	aptEnv := newAptEnv(t, root, h.server.URL+"/team-proxy/debian", arch)

	// apt-get update: pulls Release + Packages through the proxy.
	aptEnv.run(t, aptGet, "update")

	// apt-get download hello: pulls the .deb through the proxy into a download dir.
	downloadDir := filepath.Join(root, "downloads")
	if err := os.MkdirAll(downloadDir, 0o755); err != nil {
		t.Fatalf("mkdir downloads: %v", err)
	}
	aptEnv.runIn(t, downloadDir, aptGet, "download", "hello")

	debName := fmt.Sprintf("hello_2.10-2_%s.deb", arch)
	gotDeb, err := os.ReadFile(filepath.Join(downloadDir, debName))
	if err != nil {
		// apt-get download mangles the filename in some versions; accept any .deb.
		entries, _ := os.ReadDir(downloadDir)
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
			if strings.HasSuffix(e.Name(), ".deb") {
				gotDeb, err = os.ReadFile(filepath.Join(downloadDir, e.Name()))
			}
		}
		if err != nil {
			t.Fatalf("apt-get download did not produce a .deb (dir has %v): %v", names, err)
		}
	}
	if !bytes.Equal(gotDeb, repo.deb) {
		t.Fatalf("downloaded .deb (%d bytes) does not match upstream (%d bytes)", len(gotDeb), len(repo.deb))
	}

	// The pull-through filled the cache: a direct GET to open-artifact now serves
	// the .deb without contacting upstream.
	up.setStatus(repo.debPath, http.StatusInternalServerError)
	resp := get(t, h, "/team-proxy/debian/"+repo.debPath)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cached .deb GET status = %d, want 200: %s", resp.StatusCode, readResp(t, resp))
	}
	if body := []byte(readResp(t, resp)); !bytes.Equal(body, repo.deb) {
		t.Fatalf("cached .deb body (%d bytes) does not match upstream", len(body))
	}
}

func requireApt(t *testing.T) string {
	t.Helper()
	aptGet, err := exec.LookPath("apt-get")
	if err != nil {
		t.Skipf("apt-get is not available: %v", err)
	}
	if _, err := exec.LookPath("dpkg"); err != nil {
		t.Skipf("dpkg is not available: %v", err)
	}
	return aptGet
}

func dpkgArch(t *testing.T) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "dpkg", "--print-architecture").Output()
	if err != nil {
		t.Skipf("dpkg --print-architecture failed: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// aptRepo is a minimal, self-consistent APT repository: a single .deb in pool/
// plus the Packages and Release indexes whose checksums apt verifies.
type aptRepo struct {
	files   map[string][]byte
	deb     []byte
	debPath string
}

func newAptRepo(t *testing.T, arch string) *aptRepo {
	t.Helper()
	deb := buildDeb(t, "hello", "2.10-2", arch)
	debPath := fmt.Sprintf("pool/main/h/hello/hello_2.10-2_%s.deb", arch)

	packages := fmt.Sprintf(`Package: hello
Version: 2.10-2
Architecture: %s
Maintainer: Test <test@example.com>
Filename: %s
Size: %d
MD5sum: %s
SHA256: %s
Description: test package for open-artifact debian proxy
`, arch, debPath, len(deb), md5Hex(deb), sha256Hex(deb))
	packagesBytes := []byte(packages)

	packagesRepoPath := fmt.Sprintf("main/binary-%s/Packages", arch)
	release := fmt.Sprintf(`Origin: open-artifact-test
Label: open-artifact-test
Suite: stable
Codename: stable
Architectures: %s
Components: main
Date: %s
SHA256:
 %s %d %s
MD5Sum:
 %s %d %s
`, arch, time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 UTC"),
		sha256Hex(packagesBytes), len(packagesBytes), packagesRepoPath,
		md5Hex(packagesBytes), len(packagesBytes), packagesRepoPath)

	return &aptRepo{
		files: map[string][]byte{
			"dists/stable/Release":             []byte(release),
			"dists/stable/" + packagesRepoPath: packagesBytes,
			debPath:                            deb,
		},
		deb:     deb,
		debPath: debPath,
	}
}

// buildDeb assembles a minimal but structurally valid .deb (an ar archive of
// debian-binary, control.tar.gz, data.tar.gz). apt-get download only hash-checks
// the bytes against the Packages index, so the internal contents need only be
// well-formed.
func buildDeb(t *testing.T, name, version, arch string) []byte {
	t.Helper()
	control := fmt.Sprintf(`Package: %s
Version: %s
Architecture: %s
Maintainer: Test <test@example.com>
Installed-Size: 1
Section: misc
Priority: optional
Description: test package for open-artifact debian proxy
`, name, version, arch)

	controlTar := gzTar(t, map[string]string{"./control": control})
	dataTar := gzTar(t, map[string]string{"./usr/share/doc/hello/README": "hello\n"})

	var buf bytes.Buffer
	buf.WriteString("!<arch>\n")
	writeArMember(&buf, "debian-binary", []byte("2.0\n"))
	writeArMember(&buf, "control.tar.gz", controlTar)
	writeArMember(&buf, "data.tar.gz", dataTar)
	return buf.Bytes()
}

func writeArMember(buf *bytes.Buffer, name string, data []byte) {
	header := make([]byte, 60)
	for i := range header {
		header[i] = ' '
	}
	copy(header[0:16], name)
	copy(header[16:28], "0")      // mtime
	copy(header[28:34], "0")      // uid
	copy(header[34:40], "0")      // gid
	copy(header[40:48], "100644") // mode
	copy(header[48:58], strconv.Itoa(len(data)))
	header[58] = 0x60
	header[59] = 0x0A
	buf.Write(header)
	buf.Write(data)
	if len(data)%2 == 1 {
		buf.WriteByte('\n')
	}
}

func gzTar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(content)),
		}); err != nil {
			t.Fatalf("tar header %q: %v", name, err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("tar write %q: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func md5Hex(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// aptEnv runs apt-get with every directory redirected into a temp root so it
// works without root privileges.
type aptEnv struct {
	opts []string
}

func newAptEnv(t *testing.T, root, repoURL, arch string) *aptEnv {
	t.Helper()
	listsDir := filepath.Join(root, "lists")
	archivesDir := filepath.Join(root, "archives")
	etcDir := filepath.Join(root, "etc")
	for _, d := range []string{
		filepath.Join(listsDir, "partial"),
		filepath.Join(archivesDir, "partial"),
		etcDir,
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	sourcesFile := filepath.Join(etcDir, "sources.list")
	if err := os.WriteFile(sourcesFile,
		[]byte(fmt.Sprintf("deb [trusted=yes arch=%s] %s stable main\n", arch, repoURL)), 0o644); err != nil {
		t.Fatalf("write sources.list: %v", err)
	}
	statusFile := filepath.Join(etcDir, "status")
	if err := os.WriteFile(statusFile, nil, 0o644); err != nil {
		t.Fatalf("write status: %v", err)
	}

	return &aptEnv{opts: []string{
		"-o", "Dir::Etc::sourcelist=" + sourcesFile,
		"-o", "Dir::Etc::sourceparts=-",
		"-o", "Dir::State::Lists=" + listsDir,
		"-o", "Dir::State::status=" + statusFile,
		"-o", "Dir::Cache=" + filepath.Join(root, "cache"),
		"-o", "Dir::Cache::Archives=" + archivesDir,
		"-o", "Acquire::Languages=none",
		"-o", "APT::Architecture=" + arch,
		"-o", "APT::Get::List-Cleanup=0",
		"-o", "Acquire::AllowInsecureRepositories=true",
	}}
}

func (e *aptEnv) run(t *testing.T, aptGet string, args ...string) {
	t.Helper()
	e.runIn(t, "", aptGet, args...)
}

func (e *aptEnv) runIn(t *testing.T, dir, aptGet string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	full := append(append([]string{}, e.opts...), args...)
	cmd := exec.CommandContext(ctx, aptGet, full...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("apt-get %v failed: %v\n%s", args, err, out.String())
	}
}
