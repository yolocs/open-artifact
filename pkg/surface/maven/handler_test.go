package maven_test

import (
	"crypto/sha1"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"gocloud.dev/blob"
	"gocloud.dev/blob/fileblob"
	"gocloud.dev/blob/memblob"

	"github.com/yolocs/open-artifact/pkg/auth"
	"github.com/yolocs/open-artifact/pkg/namespace"
	"github.com/yolocs/open-artifact/pkg/surface/integrationtest"
	"github.com/yolocs/open-artifact/pkg/surface/maven"
)

type backend struct {
	name string
	open func(t *testing.T) *blob.Bucket
}

func backends() []backend {
	return []backend{
		{
			name: "memblob",
			open: func(t *testing.T) *blob.Bucket {
				t.Helper()
				b := memblob.OpenBucket(nil)
				t.Cleanup(func() { b.Close() })
				return b
			},
		},
		{
			name: "fileblob",
			open: func(t *testing.T) *blob.Bucket {
				t.Helper()
				b, err := fileblob.OpenBucket(t.TempDir(), nil)
				if err != nil {
					t.Fatalf("fileblob.OpenBucket: %v", err)
				}
				t.Cleanup(func() { b.Close() })
				return b
			},
		},
	}
}

type harness struct {
	server *httptest.Server
	client *http.Client
}

func newHarness(t *testing.T, b *blob.Bucket, cfg maven.Config) *harness {
	t.Helper()
	ctx := t.Context()
	catalog, err := namespace.NewStore(b, "")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	for _, ns := range []*namespace.Namespace{
		integrationtest.HostedAnonymous("team-a"),
		integrationtest.HostedAnonymous("team-b"),
		integrationtest.DenyAll("team-deny"),
		integrationtest.ReadOnlyAnonymous("team-readonly"),
		integrationtest.ProxyAnonymous("team-proxy", "https://repo1.maven.org/maven2/"),
	} {
		if err := integrationtest.SeedNamespace(ctx, catalog, ns); err != nil {
			t.Fatalf("SeedNamespace(%s): %v", ns.Name, err)
		}
	}
	reg, err := namespace.NewRegistry(b, "", catalog)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	srv := httptest.NewServer(maven.Handler(reg, auth.AlwaysAnonymous{}, cfg))
	t.Cleanup(srv.Close)
	return &harness{server: srv, client: srv.Client()}
}

func put(t *testing.T, h *harness, path, body string) *http.Response {
	t.Helper()
	return request(t, h, http.MethodPut, path, strings.NewReader(body))
}

func post(t *testing.T, h *harness, path, body string) *http.Response {
	t.Helper()
	return request(t, h, http.MethodPost, path, strings.NewReader(body))
}

func get(t *testing.T, h *harness, path string) *http.Response {
	t.Helper()
	return request(t, h, http.MethodGet, path, nil)
}

func head(t *testing.T, h *harness, path string) *http.Response {
	t.Helper()
	return request(t, h, http.MethodHead, path, nil)
}

func request(t *testing.T, h *harness, method, path string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, h.server.URL+path, body)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("Do %s: %v", method, err)
	}
	return resp
}

func readResp(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	return string(b)
}

func sha1Hex(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestHostedReleaseUploadReadChecksumAndImmutability(t *testing.T) {
	t.Parallel()

	for _, be := range backends() {
		be := be
		t.Run(be.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, be.open(t), maven.Config{})
			path := "/team-a/maven2/com/example/demo/1.0.0/demo-1.0.0.jar"

			created := put(t, h, path, "jar bytes")
			if got := created.StatusCode; got != http.StatusCreated {
				t.Fatalf("PUT status = %d, want %d: %s", got, http.StatusCreated, readResp(t, created))
			}
			_ = readResp(t, created)

			dup := put(t, h, path, "new bytes")
			if got := dup.StatusCode; got != http.StatusConflict {
				t.Fatalf("duplicate status = %d, want %d: %s", got, http.StatusConflict, readResp(t, dup))
			}
			_ = readResp(t, dup)

			checksum := put(t, h, path+".sha1", sha1Hex("jar bytes")+"  demo-1.0.0.jar\n")
			if got := checksum.StatusCode; got != http.StatusCreated {
				t.Fatalf("checksum status = %d, want %d: %s", got, http.StatusCreated, readResp(t, checksum))
			}
			_ = readResp(t, checksum)

			got := get(t, h, path)
			if got.StatusCode != http.StatusOK {
				t.Fatalf("GET status = %d, want %d: %s", got.StatusCode, http.StatusOK, readResp(t, got))
			}
			if ct := got.Header.Get("Content-Type"); ct != "application/octet-stream" {
				t.Fatalf("content type = %q, want application/octet-stream", ct)
			}
			if diff := cmp.Diff("jar bytes", readResp(t, got)); diff != "" {
				t.Fatalf("GET body mismatch (-want +got):\n%s", diff)
			}

			gotChecksum := get(t, h, path+".sha1")
			if gotChecksum.StatusCode != http.StatusOK {
				t.Fatalf("checksum GET status = %d: %s", gotChecksum.StatusCode, readResp(t, gotChecksum))
			}
			if body := readResp(t, gotChecksum); body != sha1Hex("jar bytes")+"  demo-1.0.0.jar\n" {
				t.Fatalf("checksum body = %q", body)
			}

			headResp := head(t, h, path)
			defer headResp.Body.Close()
			if got := headResp.StatusCode; got != http.StatusOK {
				t.Fatalf("HEAD status = %d, want %d", got, http.StatusOK)
			}
			if body, err := io.ReadAll(headResp.Body); err != nil || len(body) != 0 {
				t.Fatalf("HEAD body len = %d, err = %v; want empty nil", len(body), err)
			}
		})
	}
}

func TestHostedSnapshotRedeployAndConcurrentFiles(t *testing.T) {
	t.Parallel()

	h := newHarness(t, memblob.OpenBucket(nil), maven.Config{})
	path := "/team-a/maven2/com/example/demo/1.0.0-SNAPSHOT/demo-1.0.0-20260101.123456-1.jar"
	first := put(t, h, path, "first")
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("first status = %d: %s", first.StatusCode, readResp(t, first))
	}
	_ = readResp(t, first)
	second := put(t, h, path, "second")
	if second.StatusCode != http.StatusCreated {
		t.Fatalf("second status = %d: %s", second.StatusCode, readResp(t, second))
	}
	_ = readResp(t, second)
	if body := readResp(t, get(t, h, path)); body != "second" {
		t.Fatalf("snapshot body = %q, want second", body)
	}

	var wg sync.WaitGroup
	for _, file := range []string{"demo-1.0.0-20260101.123456-2.jar", "demo-1.0.0-20260101.123456-2.pom"} {
		file := file
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := put(t, h, "/team-a/maven2/com/example/demo/1.0.0-SNAPSHOT/"+file, file)
			if resp.StatusCode != http.StatusCreated {
				t.Errorf("concurrent PUT %s status = %d: %s", file, resp.StatusCode, readResp(t, resp))
				return
			}
			_ = readResp(t, resp)
		}()
	}
	wg.Wait()
}

func TestHostedMetadataArchetypeAndPostUpload(t *testing.T) {
	t.Parallel()

	h := newHarness(t, memblob.OpenBucket(nil), maven.Config{})
	metadataPath := "/team-a/maven2/com/example/demo/maven-metadata.xml"
	metadata := "<metadata><groupId>com.example</groupId><artifactId>demo</artifactId></metadata>"
	resp := post(t, h, metadataPath, metadata)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("metadata POST status = %d: %s", resp.StatusCode, readResp(t, resp))
	}
	_ = readResp(t, resp)
	dup := put(t, h, metadataPath, metadata)
	if dup.StatusCode != http.StatusConflict {
		t.Fatalf("metadata duplicate status = %d, want conflict: %s", dup.StatusCode, readResp(t, dup))
	}
	_ = readResp(t, dup)
	gotMetadata := get(t, h, metadataPath)
	if gotMetadata.StatusCode != http.StatusOK {
		t.Fatalf("metadata GET status = %d: %s", gotMetadata.StatusCode, readResp(t, gotMetadata))
	}
	if ct := gotMetadata.Header.Get("Content-Type"); ct != "application/xml" {
		t.Fatalf("metadata content type = %q, want application/xml", ct)
	}
	if body := readResp(t, gotMetadata); body != metadata {
		t.Fatalf("metadata body = %q", body)
	}

	catalogPath := "/team-a/maven2/archetype-catalog.xml"
	catalog := "<archetype-catalog/>"
	catResp := put(t, h, catalogPath, catalog)
	if catResp.StatusCode != http.StatusCreated {
		t.Fatalf("catalog PUT status = %d: %s", catResp.StatusCode, readResp(t, catResp))
	}
	_ = readResp(t, catResp)
	if body := readResp(t, get(t, h, catalogPath)); body != catalog {
		t.Fatalf("catalog body = %q", body)
	}
}

func TestHostedRejectsChecksumBeforeTargetMismatchAndInvalidPaths(t *testing.T) {
	t.Parallel()

	h := newHarness(t, memblob.OpenBucket(nil), maven.Config{})
	path := "/team-a/maven2/com/example/demo/1.0.0/demo-1.0.0.jar"
	missingTarget := put(t, h, path+".sha1", sha1Hex("missing"))
	if missingTarget.StatusCode != http.StatusConflict {
		t.Fatalf("checksum before target status = %d, want conflict: %s", missingTarget.StatusCode, readResp(t, missingTarget))
	}
	_ = readResp(t, missingTarget)

	created := put(t, h, path, "jar bytes")
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("artifact status = %d: %s", created.StatusCode, readResp(t, created))
	}
	_ = readResp(t, created)
	mismatch := put(t, h, path+".sha1", "0000")
	if mismatch.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("checksum mismatch status = %d, want 422: %s", mismatch.StatusCode, readResp(t, mismatch))
	}
	_ = readResp(t, mismatch)

	bad := put(t, h, "/team-a/maven2/com/.hidden/demo/1.0.0/demo.jar", "bad")
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad path status = %d, want 400: %s", bad.StatusCode, readResp(t, bad))
	}
	_ = readResp(t, bad)
}

func TestHostedAuthModeLimitsUploadCapAndNamespaceIsolation(t *testing.T) {
	t.Parallel()

	h := newHarness(t, memblob.OpenBucket(nil), maven.Config{MaxUploadBytes: 4})
	path := "/team-a/maven2/com/example/demo/1.0.0/demo-1.0.0.jar"
	tooLarge := put(t, h, path, "12345")
	if tooLarge.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("large upload status = %d, want 413: %s", tooLarge.StatusCode, readResp(t, tooLarge))
	}
	_ = readResp(t, tooLarge)

	ok := put(t, h, path, "1234")
	if ok.StatusCode != http.StatusCreated {
		t.Fatalf("upload status = %d: %s", ok.StatusCode, readResp(t, ok))
	}
	_ = readResp(t, ok)
	isolated := get(t, h, "/team-b/maven2/com/example/demo/1.0.0/demo-1.0.0.jar")
	if isolated.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-namespace GET status = %d, want 404: %s", isolated.StatusCode, readResp(t, isolated))
	}
	_ = readResp(t, isolated)

	readDenied := get(t, h, "/team-deny/maven2/com/example/demo/1.0.0/demo-1.0.0.jar")
	if readDenied.StatusCode != http.StatusForbidden {
		t.Fatalf("deny read status = %d, want 403: %s", readDenied.StatusCode, readResp(t, readDenied))
	}
	_ = readResp(t, readDenied)
	writeDenied := put(t, h, "/team-readonly/maven2/com/example/demo/1.0.0/demo-1.0.0.jar", "1234")
	if writeDenied.StatusCode != http.StatusForbidden {
		t.Fatalf("readonly write status = %d, want 403: %s", writeDenied.StatusCode, readResp(t, writeDenied))
	}
	_ = readResp(t, writeDenied)
	proxyWrite := put(t, h, "/team-proxy/maven2/com/example/demo/1.0.0/demo-1.0.0.jar", "1234")
	if proxyWrite.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("proxy write status = %d, want 405: %s", proxyWrite.StatusCode, readResp(t, proxyWrite))
	}
	_ = readResp(t, proxyWrite)
}

func TestAllowOverwritePermitsReleaseRedeploy(t *testing.T) {
	t.Parallel()

	h := newHarness(t, memblob.OpenBucket(nil), maven.Config{AllowOverwrite: true})
	path := "/team-a/maven2/com/example/demo/1.0.0/demo-1.0.0.jar"
	first := put(t, h, path, "first")
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("first status = %d: %s", first.StatusCode, readResp(t, first))
	}
	_ = readResp(t, first)
	second := put(t, h, path, "second")
	if second.StatusCode != http.StatusCreated {
		t.Fatalf("second status = %d: %s", second.StatusCode, readResp(t, second))
	}
	_ = readResp(t, second)
	if body := readResp(t, get(t, h, path)); body != "second" {
		t.Fatalf("body = %q, want second", body)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	t.Parallel()

	h := newHarness(t, memblob.OpenBucket(nil), maven.Config{})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodDelete, h.server.URL+"/team-a/maven2/com/example/demo/1.0.0/demo-1.0.0.jar", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("Do DELETE: %v", err)
	}
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE status = %d, want 405: %s", resp.StatusCode, readResp(t, resp))
	}
	_ = readResp(t, resp)
}
