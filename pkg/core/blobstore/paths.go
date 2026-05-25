package blobstore

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/yolocs/open-artifact/pkg/core"
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
	// filesDir is the per-level file directory. Keeping files under a dot-entry
	// prevents namespace files from looking like packages and package files from
	// looking like versions.
	filesDir = ".files/"
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

// encodeSegment renders a user-provided name (package, version, file, tag, or
// cache key) as a single path-safe bucket segment. The Store — not the caller —
// owns this: a surface may pass whatever name a client sends, subject to
// validateName. It uses url.QueryEscape, which escapes aggressively (every
// reserved or non-alphanumeric byte except "-_.~"), keeping the segment broadly
// compatible across blob backends and turning a "/" (npm scoped names like
// "@scope/name", Maven coordinates) into "%2F" so the name stays one bucket
// child rather than nesting.
//
// A leading "." is not handled here — such names are rejected by validateName
// before they reach storage — and a valid name (never starting with ".") never
// QueryEscapes to a leading ".", so an encoded segment never collides with the
// reserved .meta/.tags/.cache dot-files.
func encodeSegment(name string) string {
	return url.QueryEscape(name)
}

// validateName rejects a caller-provided name that is empty or begins with "."
// A leading dot is reserved for Store-owned objects (.meta/.tags/.cache) and a
// dot-prefixed name would be hidden from listings or collide with them, so the
// Store refuses such input outright rather than escaping it. It returns
// core.ErrInvalidName (wrapped) on failure.
func validateName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: empty name", core.ErrInvalidName)
	}
	if strings.HasPrefix(name, ".") {
		return fmt.Errorf("%w: %q must not start with '.'", core.ErrInvalidName, name)
	}
	return nil
}

// firstErr returns the first non-nil error, or nil. It chains a handle's name
// validation with its parent's, so a child handle reports an invalid ancestor.
func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
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

func filesPrefix(dir string) string {
	return dir + filesDir
}

func levelFilePath(dir, name string) string {
	return filesPrefix(dir) + encodeSegment(name)
}

func levelFileMetaPath(dir, name string) string {
	return filesPrefix(dir) + metaFilePrefix + encodeSegment(name)
}

func storeFilePath(scope, file string) string {
	return levelFilePath(scopePrefix(scope), file)
}

func storeFileMetaPath(scope, file string) string {
	return levelFileMetaPath(scopePrefix(scope), file)
}

func packageFilePath(scope, pkg, file string) string {
	return levelFilePath(packagePrefix(scope, pkg), file)
}

func packageFileMetaPath(scope, pkg, file string) string {
	return levelFileMetaPath(packagePrefix(scope, pkg), file)
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
	return levelFilePath(versionPrefix(scope, pkg, version), file)
}

// fileMetaPath returns the path of a file's .meta.<file> sidecar.
func fileMetaPath(scope, pkg, version, file string) string {
	return levelFileMetaPath(versionPrefix(scope, pkg, version), file)
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
