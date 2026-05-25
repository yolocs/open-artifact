package maven

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestParsePath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		path    string
		want    requestPath
		wantErr bool
	}{
		{
			name: "artifact",
			path: "/team-a/maven2/com/example/demo/1.0.0/demo-1.0.0.jar",
			want: requestPath{
				Namespace:  "team-a",
				Kind:       pathArtifact,
				GroupPath:  []string{"com", "example"},
				GroupID:    "com.example",
				ArtifactID: "demo",
				Package:    "com/example/demo",
				Version:    "1.0.0",
				File:       "demo-1.0.0.jar",
			},
		},
		{
			name: "checksum companion",
			path: "/team-a/maven2/com/example/demo/1.0.0/demo-1.0.0.jar.sha256",
			want: requestPath{
				Namespace:  "team-a",
				Kind:       pathArtifact,
				GroupPath:  []string{"com", "example"},
				GroupID:    "com.example",
				ArtifactID: "demo",
				Package:    "com/example/demo",
				Version:    "1.0.0",
				File:       "demo-1.0.0.jar.sha256",
				Checksum:   checksumSHA256,
				TargetFile: "demo-1.0.0.jar",
			},
		},
		{
			name: "artifact metadata",
			path: "/team-a/maven2/com/example/demo/maven-metadata.xml",
			want: requestPath{
				Namespace:  "team-a",
				Kind:       pathArtifactMetadata,
				GroupPath:  []string{"com", "example"},
				GroupID:    "com.example",
				ArtifactID: "demo",
				Package:    "com/example/demo",
				File:       metadataFile,
			},
		},
		{
			name: "version metadata checksum",
			path: "/team-a/maven2/com/example/demo/1.0.0-SNAPSHOT/maven-metadata.xml.sha1",
			want: requestPath{
				Namespace:  "team-a",
				Kind:       pathVersionMetadata,
				GroupPath:  []string{"com", "example"},
				GroupID:    "com.example",
				ArtifactID: "demo",
				Package:    "com/example/demo",
				Version:    "1.0.0-SNAPSHOT",
				File:       "maven-metadata.xml.sha1",
				Checksum:   checksumSHA1,
				TargetFile: metadataFile,
			},
		},
		{
			name: "archetype catalog",
			path: "/team-a/maven2/archetype-catalog.xml",
			want: requestPath{
				Namespace: "team-a",
				Kind:      pathArchetypeCatalog,
				File:      archetypeFile,
			},
		},
		{name: "missing maven2 root", path: "/team-a/packages/demo", wantErr: true},
		{name: "empty segment", path: "/team-a/maven2/com//demo/1.0.0/demo.jar", wantErr: true},
		{name: "dot segment", path: "/team-a/maven2/com/./demo/1.0.0/demo.jar", wantErr: true},
		{name: "dotdot segment", path: "/team-a/maven2/com/../demo/1.0.0/demo.jar", wantErr: true},
		{name: "leading dot segment", path: "/team-a/maven2/com/.hidden/demo/1.0.0/demo.jar", wantErr: true},
		{name: "bad character", path: "/team-a/maven2/com/example/demo/1.0.0/demo 1.0.0.jar", wantErr: true},
		{name: "malformed escape", path: "/team-a/maven2/com/%zz/demo/1.0.0/demo.jar", wantErr: true},
		{
			name: "underscore version prefix",
			path: "/team-a/maven2/com/example/demo/__open_artifact_user/demo.jar",
			want: requestPath{
				Namespace:  "team-a",
				Kind:       pathArtifact,
				GroupPath:  []string{"com", "example"},
				GroupID:    "com.example",
				ArtifactID: "demo",
				Package:    "com/example/demo",
				Version:    "__open_artifact_user",
				File:       "demo.jar",
			},
		},
		{name: "not enough segments", path: "/team-a/maven2/com/example/demo.jar", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parsePath(tc.path)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parsePath(%q) = nil error, want error", tc.path)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePath(%q): %v", tc.path, err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("parsePath mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestSnapshotVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		version string
		want    bool
	}{
		{version: "1.0.0", want: false},
		{version: "1.0.0-SNAPSHOT", want: true},
		{version: "1.0.0-snapshot", want: true},
		{version: "1.0.0-SNAPSHOT-build", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.version, func(t *testing.T) {
			t.Parallel()
			if got := isSnapshotVersion(tc.version); got != tc.want {
				t.Fatalf("isSnapshotVersion(%q) = %v, want %v", tc.version, got, tc.want)
			}
		})
	}
}

func TestChecksumVerification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		algorithm  checksumAlgorithm
		targetBody string
		declared   string
		wantErr    bool
	}{
		{name: "md5", algorithm: checksumMD5, targetBody: "artifact", declared: "8e5b948a454515dbabfc7eb718daa52f"},
		{name: "sha1 with trailing filename", algorithm: checksumSHA1, targetBody: "artifact", declared: "1e5dcbb59b753cb1d46e234d8f6180285b8b86ad  artifact.jar\n"},
		{name: "sha256", algorithm: checksumSHA256, targetBody: "artifact", declared: "c7c5c1d70c5dec4416ab6158afd0b223ef40c29b1dc1f97ed9428b94d4cadb1c"},
		{name: "sha512", algorithm: checksumSHA512, targetBody: "artifact", declared: "14697440701c3885f7c8d5faa59f336b471ca86332034eff0d3fddc02dc9b18b8356e840db54823c8fd2f2cbd0906969cf132cf8bb9c73dc769b4ffd817bd23d"},
		{name: "mismatch", algorithm: checksumSHA1, targetBody: "artifact", declared: "0000", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := verifyChecksum(tc.algorithm, strings.NewReader(tc.targetBody), strings.NewReader(tc.declared))
			if tc.wantErr != (err != nil) {
				t.Fatalf("verifyChecksum() err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}
