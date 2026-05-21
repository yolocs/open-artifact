package blobstore

import (
	"strings"
	"testing"
)

func TestPathHelpers(t *testing.T) {
	t.Parallel()

	const scope = "pypi/global"
	const pkg = "requests"
	const ver = "2.31.0"
	const fname = "requests-2.31.0-py3-none-any.whl"

	cases := []struct {
		name string
		got  string
		want string
	}{
		{"scopePrefix", scopePrefix(scope), "open-artifact/v1/pypi/global/"},
		{"packagePrefix", packagePrefix(scope, pkg), "open-artifact/v1/pypi/global/requests/"},
		{"packageMetaPath", packageMetaPath(scope, pkg), "open-artifact/v1/pypi/global/requests/.meta"},
		{"packageTagsPrefix", packageTagsPrefix(scope, pkg), "open-artifact/v1/pypi/global/requests/.tags/"},
		{"tagPath", tagPath(scope, pkg, "latest"), "open-artifact/v1/pypi/global/requests/.tags/latest"},
		{"versionPrefix", versionPrefix(scope, pkg, ver), "open-artifact/v1/pypi/global/requests/2.31.0/"},
		{"versionMetaPath", versionMetaPath(scope, pkg, ver), "open-artifact/v1/pypi/global/requests/2.31.0/.meta"},
		{"filePath", filePath(scope, pkg, ver, fname), "open-artifact/v1/pypi/global/requests/2.31.0/" + fname},
		{"fileMetaPath", fileMetaPath(scope, pkg, ver, fname), "open-artifact/v1/pypi/global/requests/2.31.0/.meta." + fname},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.got != tc.want {
				t.Errorf("got %q, want %q", tc.got, tc.want)
			}
		})
	}
}

func TestScopePrefixNormalization(t *testing.T) {
	t.Parallel()

	cases := []struct {
		scope string
		want  string
	}{
		{"", "open-artifact/v1/"},
		{"/", "open-artifact/v1/"},
		{"pypi/global", "open-artifact/v1/pypi/global/"},
		{"/pypi/global/", "open-artifact/v1/pypi/global/"},
	}
	for _, tc := range cases {
		if got := scopePrefix(tc.scope); got != tc.want {
			t.Errorf("scopePrefix(%q) = %q, want %q", tc.scope, got, tc.want)
		}
	}
}

func TestPackageNameEncoding(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		enc  string
	}{
		{"requests", "requests"},
		{"@scope/name", "@scope%2Fname"},
		{"a/b/c", "a%2Fb%2Fc"},
		{"with space", "with%20space"},
		{"100%", "100%25"},
		{"%2F", "%252F"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := encodePkgName(tc.name)
			if got != tc.enc {
				t.Errorf("encodePkgName(%q) = %q, want %q", tc.name, got, tc.enc)
			}
			if strings.Contains(got, "/") {
				t.Errorf("encodePkgName(%q) = %q contains a path separator", tc.name, got)
			}
			if rt := decodePkgName(got); rt != tc.name {
				t.Errorf("decodePkgName(%q) = %q, want %q", got, rt, tc.name)
			}
		})
	}
}

func TestPackagePrefixEncodesScopedName(t *testing.T) {
	t.Parallel()

	got := packageMetaPath("team-a/npm", "@scope/name")
	want := "open-artifact/v1/team-a/npm/@scope%2Fname/.meta"
	if got != want {
		t.Errorf("packageMetaPath = %q, want %q", got, want)
	}
}

func TestIsDotEntry(t *testing.T) {
	t.Parallel()

	cases := map[string]bool{
		".meta":         true,
		".meta.foo.whl": true,
		".tags":         true,
		"requests":      false,
		"2.31.0":        false,
	}
	for name, want := range cases {
		if got := isDotEntry(name); got != want {
			t.Errorf("isDotEntry(%q) = %v, want %v", name, got, want)
		}
	}
}
