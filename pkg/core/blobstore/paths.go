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
// children. User-provided names can never collide with these because
// encodeSegment escapes a leading dot (see below).
const (
	// metaName is the package- or version-level metadata envelope.
	metaName = ".meta"
	// metaFilePrefix prefixes a per-file metadata sidecar: ".meta.<file>".
	metaFilePrefix = ".meta."
	// tagsName is the package-level dist-tags directory. Each dist-tag is a
	// separate object .tags/<tag> whose content is the target version, so a
	// SetTag is a single independent write — no shared file to read-modify-write.
	tagsName = ".tags"
	// cacheDir is the per-level cache directory (opaque to the listing verbs).
	// Cache files live directly under it.
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

// encodeSegment renders any user-provided name (package, version, file, or tag)
// as a single path-safe bucket segment. The Store — not the caller — owns this:
// a surface may pass whatever name a client sends. It uses url.QueryEscape,
// which escapes aggressively (every reserved or non-alphanumeric byte except
// "-_.~"), keeping the segment broadly compatible across blob backends. Two
// hazards in particular are handled:
//
//   - A "/" (npm scoped names like "@scope/name", Maven coordinates) would
//     otherwise nest into directories and break listing; QueryEscape turns it
//     into "%2F", keeping the name one bucket child.
//   - A leading "." would otherwise collide with the reserved dot-files
//     (.meta/.tags/.cache) and be dropped from listings; we escape it to "%2E".
//     QueryEscape already escapes "%" to "%25", so the encoding stays reversible
//     and no real input can forge a "%2E"/"%2F".
//
// The result never contains "/" and never starts with ".".
func encodeSegment(name string) string {
	e := url.QueryEscape(name)
	if strings.HasPrefix(e, ".") {
		e = "%2E" + e[1:]
	}
	return e
}

// decodeSegment reverses encodeSegment for a listed child segment. A segment
// that fails to decode is returned unchanged so listings never silently drop
// objects.
func decodeSegment(seg string) string {
	if dec, err := url.QueryUnescape(seg); err == nil {
		return dec
	}
	return seg
}

// packagePrefix returns the directory prefix for a package.
func packagePrefix(scope, pkg string) string {
	return scopePrefix(scope) + encodeSegment(pkg) + "/"
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
	return packageTagsPrefix(scope, pkg) + encodeSegment(tag)
}

// versionPrefix returns the directory prefix for a version.
func versionPrefix(scope, pkg, version string) string {
	return packagePrefix(scope, pkg) + encodeSegment(version) + "/"
}

// versionMetaPath returns the path of a version's .meta object.
func versionMetaPath(scope, pkg, version string) string {
	return versionPrefix(scope, pkg, version) + metaName
}

// filePath returns the path of a file's blob.
func filePath(scope, pkg, version, file string) string {
	return versionPrefix(scope, pkg, version) + encodeSegment(file)
}

// fileMetaPath returns the path of a file's .meta.<file> sidecar.
func fileMetaPath(scope, pkg, version, file string) string {
	return versionPrefix(scope, pkg, version) + metaFilePrefix + encodeSegment(file)
}

// cacheFilePath returns the path of a cache file's blob: <dir>.cache/<name>.
// dir is a level prefix ending in "/" (scope, package, or version prefix).
func cacheFilePath(dir, name string) string {
	return dir + cacheDir + encodeSegment(name)
}

// cacheMetaPath returns the path of a cache file's .meta.<name> sidecar.
func cacheMetaPath(dir, name string) string {
	return dir + cacheDir + metaFilePrefix + encodeSegment(name)
}

// isDotEntry reports whether a listing entry name is Store-owned (leading
// "."). Listings drop these when enumerating real children.
func isDotEntry(name string) bool {
	return strings.HasPrefix(name, ".")
}
