package blobstore

import (
	"net/url"
	"strings"
)

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
	// tagsName is the package-level dist-tags directory. Each dist-tag is a
	// separate object .tags/<tag> whose content is the target version, so a
	// SetTag is a single independent write — no shared file to read-modify-write.
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

// encodePkgName renders a logical package name as a single path-safe segment.
// npm scoped names like "@scope/name" carry a "/" that would otherwise nest
// into directories and break listing; percent-encoding it (via url.PathEscape)
// keeps the package one bucket child while staying lossless.
func encodePkgName(name string) string {
	return url.PathEscape(name)
}

// decodePkgName reverses encodePkgName for a listed child segment. A segment
// that fails to decode is returned unchanged so listings never silently drop
// objects.
func decodePkgName(seg string) string {
	if dec, err := url.PathUnescape(seg); err == nil {
		return dec
	}
	return seg
}

// packagePrefix returns the directory prefix for a package.
func packagePrefix(scope, pkg string) string {
	return scopePrefix(scope) + encodePkgName(pkg) + "/"
}

// packageMetaPath returns the path of a package's .meta object.
func packageMetaPath(scope, pkg string) string {
	return packagePrefix(scope, pkg) + metaName
}

// packageTagsPrefix returns the directory prefix holding a package's dist-tags.
func packageTagsPrefix(scope, pkg string) string {
	return packagePrefix(scope, pkg) + tagsName + "/"
}

// tagPath returns the path of a single dist-tag object, whose content is the
// target version string.
func tagPath(scope, pkg, tag string) string {
	return packageTagsPrefix(scope, pkg) + tag
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
