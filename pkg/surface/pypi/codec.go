package pypi

import (
	"fmt"
	"regexp"
	"strings"
)

const (
	maxPackageLength  = 256
	maxVersionLength  = 128
	maxFilenameLength = 256
)

var (
	// pep503Separators matches a run of ".", "-", or "_". Per PEP 503 any such
	// run in a project name collapses to a single "-".
	pep503Separators = regexp.MustCompile(`[-_.]+`)

	// pkgNameRe validates a raw (pre-normalization) PyPI project name per the
	// core-metadata spec: alphanumerics at the ends, with "-_." allowed inside.
	// https://packaging.python.org/specifications/core-metadata/#name
	pkgNameRe = regexp.MustCompile(`(?i)^([a-z0-9]|[a-z0-9][a-z0-9._-]*[a-z0-9])$`)

	// filenameRe gates a distribution filename: ASCII letters, digits, dot,
	// underscore, plus, hyphen. Path separators and anything exotic are
	// rejected so the value is safe as an on-bucket path segment.
	filenameRe = regexp.MustCompile(`^[A-Za-z0-9._+\-]+$`)

	// versionRe is permissive — PEP 440 is broad — but bars characters that
	// would be unsafe in a path segment.
	versionRe = regexp.MustCompile(`^[A-Za-z0-9._+!\-]+$`)
)

// normalize implements the PEP 503 name-normalization algorithm: lowercase,
// then collapse every run of ".", "-", or "_" to a single "-". "Foo_Bar",
// "foo-bar", and "foo.bar" all normalize to "foo-bar". The surface stores and
// looks packages up under the normalized name so a publish of "Foo_Bar" is
// found by "pip install foo-bar".
func normalize(name string) string {
	return pep503Separators.ReplaceAllString(strings.ToLower(name), "-")
}

// validatePackageName checks a raw project name and returns its PEP 503
// normalized form. A leading dot is rejected explicitly: the Store reserves
// leading-dot names at every directory level and silently drops them from
// listings, so accepting one here would let a publish vanish.
func validatePackageName(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("pypi: empty package name")
	}
	if len(raw) > maxPackageLength {
		return "", fmt.Errorf("pypi: package name too long")
	}
	if strings.HasPrefix(raw, ".") {
		return "", fmt.Errorf("pypi: package name may not start with %q", ".")
	}
	if !pkgNameRe.MatchString(raw) {
		return "", fmt.Errorf("pypi: invalid package name %q", raw)
	}
	return normalize(raw), nil
}

// validateVersion checks a version string for use as a path segment.
func validateVersion(v string) error {
	if v == "" {
		return fmt.Errorf("pypi: empty version")
	}
	if len(v) > maxVersionLength {
		return fmt.Errorf("pypi: version too long")
	}
	if strings.HasPrefix(v, ".") {
		return fmt.Errorf("pypi: version may not start with %q", ".")
	}
	if !versionRe.MatchString(v) {
		return fmt.Errorf("pypi: invalid version %q", v)
	}
	return nil
}

// validateFilename checks a distribution filename for use as a path segment.
// The leading-dot guard is the same reserved-name rule the package and version
// checks enforce.
func validateFilename(name string) error {
	if name == "" {
		return fmt.Errorf("pypi: empty filename")
	}
	if len(name) > maxFilenameLength {
		return fmt.Errorf("pypi: filename too long")
	}
	if name == "." || name == ".." || strings.HasPrefix(name, ".") {
		return fmt.Errorf("pypi: filename may not start with %q", ".")
	}
	if strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("pypi: filename may not contain a path separator")
	}
	if !filenameRe.MatchString(name) {
		return fmt.Errorf("pypi: invalid filename %q", name)
	}
	return nil
}

// parseFilenameVersion extracts the version segment from a PyPI distribution
// filename, given the package's PEP 503 normalized name. Wheels follow PEP 427
// (distribution-version(-build)?-pytag-abitag-platformtag.whl); sdists
// conventionally distribution-version.{tar.gz,zip,tar.bz2}. PEP 658 metadata
// sidecars append ".metadata" to a wheel name. The distribution segment may
// use "-" or "_" where the canonical name has "-", and case differs from the
// lowercased normalized name, so matching is case-insensitive across both.
func parseFilenameVersion(filename, normalizedPkg string) (string, error) {
	if filename == "" {
		return "", fmt.Errorf("pypi: empty filename")
	}
	if normalizedPkg == "" {
		return "", fmt.Errorf("pypi: empty package name")
	}
	lower := strings.ToLower(filename)
	candidates := []string{normalizedPkg}
	if strings.Contains(normalizedPkg, "-") {
		candidates = append(candidates, strings.ReplaceAll(normalizedPkg, "-", "_"))
	}
	for _, c := range candidates {
		prefix := strings.ToLower(c) + "-"
		if !strings.HasPrefix(lower, prefix) {
			continue
		}
		rest := filename[len(prefix):]
		switch {
		case strings.HasSuffix(lower, ".whl.metadata"):
			return wheelVersion(rest[:len(rest)-len(".whl.metadata")])
		case strings.HasSuffix(lower, ".whl"):
			return wheelVersion(rest[:len(rest)-len(".whl")])
		case strings.HasSuffix(lower, ".tar.gz"):
			return rest[:len(rest)-len(".tar.gz")], nil
		case strings.HasSuffix(lower, ".tar.bz2"):
			return rest[:len(rest)-len(".tar.bz2")], nil
		case strings.HasSuffix(lower, ".zip"):
			return rest[:len(rest)-len(".zip")], nil
		case strings.HasSuffix(lower, ".egg"):
			return rest[:len(rest)-len(".egg")], nil
		}
	}
	return "", fmt.Errorf("pypi: filename %q does not match package %q", filename, normalizedPkg)
}

// wheelVersion pulls the version from a wheel stem
// (version-pytag-abitag-platformtag, optionally with a build tag after the
// version). PEP 427 requires the three trailing tag segments; the version is
// everything before them.
func wheelVersion(stem string) (string, error) {
	parts := strings.Split(stem, "-")
	if len(parts) < 4 {
		return "", fmt.Errorf("pypi: wheel stem %q has too few segments", stem)
	}
	return strings.Join(parts[:len(parts)-3], "-"), nil
}
