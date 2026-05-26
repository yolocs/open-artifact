//go:build mavenupstream

package maven_test

import (
	"net/http"
	"testing"

	"gocloud.dev/blob/memblob"

	"github.com/yolocs/open-artifact/pkg/surface/maven"
)

// TestProxyLiveUpstream is a live-upstream smoke test against the real Maven
// Central (repo1.maven.org). It uses com.google.code.findbugs:jsr305:3.0.2 — a
// tiny, dependency-free, long-stable artifact — to exercise real metadata
// passthrough and artifact cache fill without a package-manager client. Run it
// with:
//
//	go test -tags=mavenupstream -run TestProxyLiveUpstream ./pkg/surface/maven
//
// Controllable scenarios (404/500, oversized metadata, filters, delay,
// checksum synthesis, negative cache) are covered by the in-process fakes in
// proxy_test.go.
func TestProxyLiveUpstream(t *testing.T) {
	t.Parallel()

	const (
		jarPath  = "/team-proxy/maven2/com/google/code/findbugs/jsr305/3.0.2/jsr305-3.0.2.jar"
		metaPath = "/team-proxy/maven2/com/google/code/findbugs/jsr305/maven-metadata.xml"
	)
	h := newProxyHarness(t, memblob.OpenBucket(nil), maven.Config{},
		proxyNS("team-proxy", "https://repo1.maven.org/maven2"))

	// Artifact-level metadata is fetched live and streamed through.
	meta := get(t, h, metaPath)
	if meta.StatusCode != http.StatusOK {
		t.Fatalf("metadata status = %d: %s", meta.StatusCode, readResp(t, meta))
	}
	if body := readResp(t, meta); len(body) == 0 {
		t.Fatalf("metadata body is empty")
	}

	// Cold artifact download pulls through and fills the cache.
	cold := get(t, h, jarPath)
	if cold.StatusCode != http.StatusOK {
		t.Fatalf("cold download status = %d: %s", cold.StatusCode, readResp(t, cold))
	}
	if body := readResp(t, cold); len(body) == 0 {
		t.Fatalf("downloaded jar is empty")
	}

	// Second download is served from the local blob cache.
	warm := get(t, h, jarPath)
	if warm.StatusCode != http.StatusOK {
		t.Fatalf("cached download status = %d: %s", warm.StatusCode, readResp(t, warm))
	}
	if body := readResp(t, warm); len(body) == 0 {
		t.Fatalf("cached jar is empty")
	}
}
