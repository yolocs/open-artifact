//go:build integration

package maven_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"gocloud.dev/blob/memblob"

	"github.com/yolocs/open-artifact/pkg/surface/maven"
)

func TestMavenDeployAndDependencyGet(t *testing.T) {
	t.Parallel()

	mvn := requireMaven(t)
	h := newHarness(t, memblob.OpenBucket(nil), maven.Config{})
	project := writeMavenProject(t, h.server.URL+"/team-a/maven2")

	deployRepo := filepath.Join(t.TempDir(), "m2-deploy")
	runCmd(t, project, mvn,
		"-B", "-ntp",
		"-Dmaven.repo.local="+deployRepo,
		"deploy",
	)

	getRepo := filepath.Join(t.TempDir(), "m2-get")
	runCmd(t, "", mvn,
		"-B", "-ntp",
		"-Dmaven.repo.local="+getRepo,
		"org.apache.maven.plugins:maven-dependency-plugin:3.8.1:get",
		"-DremoteRepositories=open-artifact::default::"+h.server.URL+"/team-a/maven2",
		"-Dartifact=com.example:demo:1.0.0",
		"-Dtransitive=false",
	)
	jar := filepath.Join(getRepo, "com", "example", "demo", "1.0.0", "demo-1.0.0.jar")
	if _, err := os.Stat(jar); err != nil {
		t.Fatalf("dependency:get did not fetch jar from open-artifact: %v", err)
	}
}

func requireMaven(t *testing.T) string {
	t.Helper()
	mvn, err := exec.LookPath("mvn")
	if err != nil {
		t.Skipf("mvn is not available: %v", err)
	}
	runCmd(t, "", mvn, "-version")
	return mvn
}

func writeMavenProject(t *testing.T, repoURL string) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pom.xml"), fmt.Sprintf(`<project xmlns="http://maven.apache.org/POM/4.0.0"
  xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"
  xsi:schemaLocation="http://maven.apache.org/POM/4.0.0 https://maven.apache.org/xsd/maven-4.0.0.xsd">
  <modelVersion>4.0.0</modelVersion>
  <groupId>com.example</groupId>
  <artifactId>demo</artifactId>
  <version>1.0.0</version>
  <name>open-artifact Maven integration demo</name>
  <properties>
    <maven.compiler.source>8</maven.compiler.source>
    <maven.compiler.target>8</maven.compiler.target>
  </properties>
  <distributionManagement>
    <repository>
      <id>open-artifact</id>
      <url>%s</url>
    </repository>
  </distributionManagement>
</project>
`, repoURL))
	writeFile(t, filepath.Join(dir, "src", "main", "java", "com", "example", "Demo.java"), `package com.example;

public final class Demo {
    private Demo() {
    }

    public static String hello() {
        return "ok";
    }
}
`)
	return dir
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func runCmd(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), mavenEnv(t)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %v: %v\nstdout:\n%s\nstderr:\n%s", name, args, err, stdout.String(), stderr.String())
	}
}

func mavenEnv(t *testing.T) []string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home")
	env := []string{
		"HOME=" + home,
		"USERPROFILE=" + home,
	}
	if runtime.GOOS == "windows" {
		env = append(env, "APPDATA="+filepath.Join(home, "AppData", "Roaming"))
	}
	return env
}
