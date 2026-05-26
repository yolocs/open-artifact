package debian

import (
	"errors"
	"testing"

	"github.com/yolocs/open-artifact/pkg/core"
	"github.com/yolocs/open-artifact/pkg/namespace"
)

func TestParsePath(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		path        string
		wantKind    pathKind
		wantRestRaw string
		wantFile    string
		wantPkg     string
		wantVersion string
	}{
		{
			name:        "inrelease index",
			path:        "/team/debian/dists/stable/InRelease",
			wantKind:    pathIndex,
			wantRestRaw: "dists/stable/InRelease",
		},
		{
			name:        "packages gz index",
			path:        "/team/debian/dists/stable/main/binary-amd64/Packages.gz",
			wantKind:    pathIndex,
			wantRestRaw: "dists/stable/main/binary-amd64/Packages.gz",
		},
		{
			name:        "binary deb",
			path:        "/team/debian/pool/main/h/hello/hello_2.10-2_amd64.deb",
			wantKind:    pathPool,
			wantRestRaw: "pool/main/h/hello/hello_2.10-2_amd64.deb",
			wantFile:    "hello_2.10-2_amd64.deb",
			wantPkg:     "hello",
			wantVersion: "2.10-2",
		},
		{
			name:        "udeb",
			path:        "/team/debian/pool/main/h/hello/hello_2.10-2_all.udeb",
			wantKind:    pathPool,
			wantRestRaw: "pool/main/h/hello/hello_2.10-2_all.udeb",
			wantFile:    "hello_2.10-2_all.udeb",
			wantPkg:     "hello",
			wantVersion: "2.10-2",
		},
		{
			name:        "dsc source",
			path:        "/team/debian/pool/main/h/hello/hello_2.10-2.dsc",
			wantKind:    pathPool,
			wantRestRaw: "pool/main/h/hello/hello_2.10-2.dsc",
			wantFile:    "hello_2.10-2.dsc",
			wantPkg:     "hello",
			wantVersion: "2.10-2",
		},
		{
			name:        "orig tarball",
			path:        "/team/debian/pool/main/h/hello/hello_2.10.orig.tar.gz",
			wantKind:    pathPool,
			wantRestRaw: "pool/main/h/hello/hello_2.10.orig.tar.gz",
			wantFile:    "hello_2.10.orig.tar.gz",
			wantPkg:     "hello",
			wantVersion: "2.10",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parsePath(tc.path)
			if err != nil {
				t.Fatalf("parsePath(%q) error: %v", tc.path, err)
			}
			if got.Kind != tc.wantKind {
				t.Errorf("Kind = %v, want %v", got.Kind, tc.wantKind)
			}
			if got.RestRaw != tc.wantRestRaw {
				t.Errorf("RestRaw = %q, want %q", got.RestRaw, tc.wantRestRaw)
			}
			if got.File != tc.wantFile {
				t.Errorf("File = %q, want %q", got.File, tc.wantFile)
			}
			if got.PkgName != tc.wantPkg {
				t.Errorf("PkgName = %q, want %q", got.PkgName, tc.wantPkg)
			}
			if got.Version != tc.wantVersion {
				t.Errorf("Version = %q, want %q", got.Version, tc.wantVersion)
			}
		})
	}
}

func TestParsePathRejects(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		path    string
		wantErr error
	}{
		{name: "relative", path: "team/debian/dists/x", wantErr: core.ErrInvalidName},
		{name: "too short", path: "/team/debian", wantErr: core.ErrInvalidName},
		{name: "wrong root", path: "/team/apt/dists/stable/Release", wantErr: core.ErrInvalidName},
		{name: "reserved namespace", path: "/admin/debian/dists/stable/Release", wantErr: namespace.ErrInvalidName},
		{name: "traversal", path: "/team/debian/dists/../etc/passwd", wantErr: core.ErrInvalidName},
		{name: "leading dot segment", path: "/team/debian/dists/.secret/Release", wantErr: core.ErrInvalidName},
		{name: "encoded separator", path: "/team/debian/pool/main/h/hello/a%2Fb.deb", wantErr: core.ErrInvalidName},
		{name: "unsupported root", path: "/team/debian/zzz/whatever", wantErr: core.ErrNotFound},
		{name: "pool too short", path: "/team/debian/pool", wantErr: core.ErrInvalidName},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := parsePath(tc.path)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("parsePath(%q) error = %v, want %v", tc.path, err, tc.wantErr)
			}
		})
	}
}

func TestContentTypes(t *testing.T) {
	t.Parallel()

	indexCases := map[string]string{
		"dists/stable/InRelease":                     "text/plain; charset=utf-8",
		"dists/stable/Release":                       "text/plain; charset=utf-8",
		"dists/stable/Release.gpg":                   "application/pgp-signature",
		"dists/stable/main/binary-amd64/Packages":    "text/plain; charset=utf-8",
		"dists/stable/main/binary-amd64/Packages.gz": "application/gzip",
		"dists/stable/main/binary-amd64/Packages.xz": "application/x-xz",
	}
	for path, want := range indexCases {
		if got := indexContentType(path); got != want {
			t.Errorf("indexContentType(%q) = %q, want %q", path, got, want)
		}
	}

	poolCases := map[string]string{
		"hello_2.10-2_amd64.deb": "application/vnd.debian.binary-package",
		"hello_2.10-2_all.udeb":  "application/vnd.debian.binary-package",
		"hello_2.10-2.dsc":       "application/octet-stream",
		"hello_2.10.orig.tar.gz": "application/octet-stream",
	}
	for file, want := range poolCases {
		if got := poolContentType(file); got != want {
			t.Errorf("poolContentType(%q) = %q, want %q", file, got, want)
		}
	}
}
