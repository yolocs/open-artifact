//go:build integration

package maven_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"gocloud.dev/blob/memblob"

	"github.com/yolocs/open-artifact/pkg/surface/maven"
)

// TestMavenProxyDependencyGet drives a real `mvn dependency:get` through a
// proxy-mode namespace whose upstream is an in-process fake Maven repository
// (newFakeMaven from proxy_test.go). It proves the pull-through path end to end
// with the actual Maven client: the first resolve fetches the pom and jar from
// the fake upstream, fills the blob cache, and a subsequent direct GET to
// open-artifact serves the cached jar.
//
// Missing checksum companions are synthesized by the proxy from the cached
// bytes, so Maven's default (warn) checksum policy is satisfied without the
// fake upstream serving any .sha1/.md5 files.
func TestMavenProxyDependencyGet(t *testing.T) {
	t.Parallel()

	mvn := requireMaven(t)

	up := newFakeMaven(t)
	pom := []byte(`<project xmlns="http://maven.apache.org/POM/4.0.0"
  xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"
  xsi:schemaLocation="http://maven.apache.org/POM/4.0.0 https://maven.apache.org/xsd/maven-4.0.0.xsd">
  <modelVersion>4.0.0</modelVersion>
  <groupId>com.example</groupId>
  <artifactId>demo</artifactId>
  <version>1.0.0</version>
</project>
`)
	up.put("com/example/demo/1.0.0/demo-1.0.0.pom", pom)
	up.put(artifactRepoPath, []byte("proxied jar bytes"))

	h := newProxyHarness(t, memblob.OpenBucket(nil), maven.Config{}, proxyNS("team-proxy", up.server.URL))

	getRepo := filepath.Join(t.TempDir(), "m2-get")
	runCmd(t, "", mvn,
		"-B", "-ntp",
		"-Dmaven.repo.local="+getRepo,
		"org.apache.maven.plugins:maven-dependency-plugin:3.8.1:get",
		"-DremoteRepositories=open-artifact::default::"+h.server.URL+"/team-proxy/maven2",
		"-Dartifact=com.example:demo:1.0.0",
		"-Dtransitive=false",
	)

	jar := filepath.Join(getRepo, "com", "example", "demo", "1.0.0", "demo-1.0.0.jar")
	if _, err := os.Stat(jar); err != nil {
		t.Fatalf("dependency:get did not fetch jar through the proxy: %v", err)
	}

	// The pull-through filled the cache: a direct GET to open-artifact now serves
	// the jar without contacting upstream.
	up.setStatus(artifactRepoPath, http.StatusInternalServerError)
	resp := get(t, h, artifactProxyPath)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cached jar GET status = %d, want 200: %s", resp.StatusCode, readResp(t, resp))
	}
	if body := readResp(t, resp); body != "proxied jar bytes" {
		t.Fatalf("cached jar body = %q", body)
	}
}
