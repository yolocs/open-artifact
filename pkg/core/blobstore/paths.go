package blobstore

import "strings"

// Root is the constant top-level prefix under which every open-artifact
// object lives. It is fixed across all scopes and backends.
const Root = "open-artifact/v1/"

// Dot-file names reserved at every directory level. A leading "." marks a
// Store-owned object; listings drop dot-entries when enumerating real
// children.
const (
	// metaName is the package- or version-level metadata envelope.
	metaName = ".meta"
	// metaFilePrefix prefixes a per-file metadata sidecar: ".meta.<file>".
	metaFilePrefix = ".meta."
	// tagsName is the package-level dist-tags / aliases object.
	tagsName = ".tags"
	// cacheDir is the package-scoped cache directory (opaque to the Store).
	cacheDir = ".cache/"
)

// scopePrefix returns the on-bucket prefix for a scope: "open-artifact/v1/<scope>/".
// A scope with surrounding slashes is normalized; an empty scope yields the
// bare Root.
func scopePrefix(scope string) string {
	scope = strings.Trim(scope, "/")
	if scope == "" {
		return Root
	}
	return Root + scope + "/"
}

// packagePrefix returns the directory prefix for a package.
func packagePrefix(scope, pkg string) string {
	return scopePrefix(scope) + pkg + "/"
}

// packageMetaPath returns the path of a package's .meta object.
func packageMetaPath(scope, pkg string) string {
	return packagePrefix(scope, pkg) + metaName
}

// packageTagsPath returns the path of a package's .tags object.
func packageTagsPath(scope, pkg string) string {
	return packagePrefix(scope, pkg) + tagsName
}

// versionPrefix returns the directory prefix for a version.
func versionPrefix(scope, pkg, version string) string {
	return packagePrefix(scope, pkg) + version + "/"
}

// versionMetaPath returns the path of a version's .meta object.
func versionMetaPath(scope, pkg, version string) string {
	return versionPrefix(scope, pkg, version) + metaName
}

// filePath returns the path of a file's blob.
func filePath(scope, pkg, version, file string) string {
	return versionPrefix(scope, pkg, version) + file
}

// fileMetaPath returns the path of a file's .meta.<file> sidecar.
func fileMetaPath(scope, pkg, version, file string) string {
	return versionPrefix(scope, pkg, version) + metaFilePrefix + file
}

// isDotEntry reports whether a listing entry name is Store-owned (leading
// "."). Listings drop these when enumerating real children.
func isDotEntry(name string) bool {
	return strings.HasPrefix(name, ".")
}
