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
		{"cacheFilePath store", cacheFilePath(scopePrefix(scope), "simple:requests"), "open-artifact/v1/pypi/global/.cache/simple%3Arequests"},
		{"cacheFilePath package", cacheFilePath(packagePrefix(scope, pkg), "simple"), "open-artifact/v1/pypi/global/requests/.cache/simple"},
		{"cacheMetaPath", cacheMetaPath(packagePrefix(scope, pkg), "simple"), "open-artifact/v1/pypi/global/requests/.cache/.meta.simple"},
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

func TestSegmentEncoding(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		enc  string
	}{
		{"requests", "requests"},
		{"2.31.0", "2.31.0"},
		{"@scope/name", "%40scope%2Fname"},
		{"a/b/c", "a%2Fb%2Fc"},
		{"with space", "with+space"},
		{"colon:name", "colon%3Aname"},
		{"100%", "100%25"},
		{"%2F", "%252F"},
		// Leading dots are escaped so user names can never masquerade as the
		// reserved .meta/.tags/.cache entries or be dropped from listings.
		{".meta", "%2Emeta"},
		{".cache", "%2Ecache"},
		{"..", "%2E."},
		{".hidden", "%2Ehidden"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := encodeSegment(tc.name)
			if got != tc.enc {
				t.Errorf("encodeSegment(%q) = %q, want %q", tc.name, got, tc.enc)
			}
			if strings.Contains(got, "/") {
				t.Errorf("encodeSegment(%q) = %q contains a path separator", tc.name, got)
			}
			if strings.HasPrefix(got, ".") {
				t.Errorf("encodeSegment(%q) = %q starts with a dot", tc.name, got)
			}
			if rt := decodeSegment(got); rt != tc.name {
				t.Errorf("decodeSegment(%q) = %q, want %q", got, rt, tc.name)
			}
		})
	}
}

func TestPackagePrefixEncodesScopedName(t *testing.T) {
	t.Parallel()

	got := packageMetaPath("team-a/npm", "@scope/name")
	want := "open-artifact/v1/team-a/npm/%40scope%2Fname/.meta"
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
