package npm

import (
	"crypto"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/yolocs/open-artifact/pkg/core"
)

// publishDoc is the CouchDB-shaped document npm sends on `npm publish`.
type publishDoc struct {
	ID          string                     `json:"_id"`
	Name        string                     `json:"name"`
	Versions    map[string]json.RawMessage `json:"versions"`
	Attachments map[string]attachment      `json:"_attachments"`
	DistTags    map[string]string          `json:"dist-tags"`
}

// attachment is one entry of the publish document's _attachments map: a
// base64-encoded tarball.
type attachment struct {
	ContentType string `json:"content_type"`
	Data        string `json:"data"`
	Length      int64  `json:"length"`
}

// Packument is the registry metadata document returned for a package read. The
// per-version objects are the publisher's stored version metadata with
// dist.tarball rewritten to this registry.
type Packument struct {
	ID       string            `json:"_id"`
	Name     string            `json:"name"`
	DistTags map[string]string `json:"dist-tags"`
	Versions map[string]any    `json:"versions"`
	Time     map[string]string `json:"time,omitempty"`
}

// maxPackageNameLen is npm's hard cap on a full package name (including the
// scope and the leading "@" / "/").
const maxPackageNameLen = 214

// PackageName is a parsed, validated npm package name. Original is the wire
// form a client sent ("left-pad" or "@scope/pkg"); Scope is empty for an
// unscoped package.
type PackageName struct {
	Original string
	Scope    string
	Name     string
}

// Scoped reports whether the package is scoped ("@scope/name").
func (p PackageName) Scoped() bool { return p.Scope != "" }

// Core renders the package name used as the core.Store package key. The format
// owns this codec: an unscoped name maps to "u/<name>" and a scoped name to
// "s/<scope>/<name>". blobstore escapes the embedded "/" into a single
// path-safe bucket segment and round-trips it losslessly through listing, so
// the two namespaces never collide and never begin with a reserved ".".
func (p PackageName) Core() string {
	if p.Scoped() {
		return "s/" + p.Scope + "/" + p.Name
	}
	return "u/" + p.Name
}

// Unscoped returns the name portion without the scope. npm names the tarball
// attachment after this ("<unscoped>-<version>.tgz") even for scoped packages.
func (p PackageName) Unscoped() string { return p.Name }

// ParsePackageName parses and validates a wire-form npm package name.
func ParsePackageName(raw string) (PackageName, error) {
	if raw == "" {
		return PackageName{}, fmt.Errorf("%w: empty package name", core.ErrInvalidName)
	}
	if len(raw) > maxPackageNameLen {
		return PackageName{}, fmt.Errorf("%w: package name exceeds %d characters", core.ErrInvalidName, maxPackageNameLen)
	}
	if strings.HasPrefix(raw, "@") {
		rest := raw[1:]
		slash := strings.IndexByte(rest, '/')
		if slash < 0 {
			return PackageName{}, fmt.Errorf("%w: scoped name must be @scope/name: %q", core.ErrInvalidName, raw)
		}
		scope, name := rest[:slash], rest[slash+1:]
		if err := validateNameSegment(scope); err != nil {
			return PackageName{}, err
		}
		if err := validateNameSegment(name); err != nil {
			return PackageName{}, err
		}
		return PackageName{Original: raw, Scope: scope, Name: name}, nil
	}
	if err := validateNameSegment(raw); err != nil {
		return PackageName{}, err
	}
	return PackageName{Original: raw, Name: raw}, nil
}

// DecodeCorePackage reverses PackageName.Core back into a wire-form name. It is
// used when reconstructing an npm name from a Store listing.
func DecodeCorePackage(coreName string) (PackageName, error) {
	switch {
	case strings.HasPrefix(coreName, "s/"):
		rest := coreName[2:]
		slash := strings.IndexByte(rest, '/')
		if slash < 0 {
			return PackageName{}, fmt.Errorf("%w: malformed scoped core name %q", core.ErrInvalidName, coreName)
		}
		scope, name := rest[:slash], rest[slash+1:]
		return PackageName{Original: "@" + scope + "/" + name, Scope: scope, Name: name}, nil
	case strings.HasPrefix(coreName, "u/"):
		name := coreName[2:]
		return PackageName{Original: name, Name: name}, nil
	default:
		return PackageName{}, fmt.Errorf("%w: unrecognized core name %q", core.ErrInvalidName, coreName)
	}
}

// validateNameSegment enforces the npm rules for a single name segment (the
// scope or the unscoped name): lowercase ASCII letters, digits, ".", "_", "-";
// no leading "." or "_"; no traversal. Uppercase, spaces, "~", "/", and percent
// signs all fall through the allow-list and are rejected.
func validateNameSegment(s string) error {
	if s == "" {
		return fmt.Errorf("%w: empty name segment", core.ErrInvalidName)
	}
	if s == "." || s == ".." {
		return fmt.Errorf("%w: path traversal is not allowed: %q", core.ErrInvalidName, s)
	}
	if strings.HasPrefix(s, ".") || strings.HasPrefix(s, "_") {
		return fmt.Errorf("%w: name segment may not begin with '.' or '_': %q", core.ErrInvalidName, s)
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return fmt.Errorf("%w: illegal character %q in name segment %q", core.ErrInvalidName, c, s)
		}
	}
	return nil
}

// ValidateVersion checks a publish version string. npm versions are semver;
// open-artifact treats the string as opaque but rejects anything that is not a
// safe single path component.
func ValidateVersion(version string) error {
	if version == "" {
		return fmt.Errorf("%w: empty version", core.ErrInvalidName)
	}
	if strings.HasPrefix(version, ".") {
		return fmt.Errorf("%w: leading dot is reserved: %q", core.ErrInvalidName, version)
	}
	if version == "." || version == ".." || path.Clean(version) != version {
		return fmt.Errorf("%w: path traversal is not allowed: %q", core.ErrInvalidName, version)
	}
	if strings.ContainsAny(version, "/\\ \t\r\n") {
		return fmt.Errorf("%w: illegal character in version %q", core.ErrInvalidName, version)
	}
	return nil
}

// ValidateTag checks a dist-tag name.
func ValidateTag(tag string) error {
	if tag == "" {
		return fmt.Errorf("%w: empty dist-tag", core.ErrInvalidName)
	}
	if strings.HasPrefix(tag, ".") {
		return fmt.Errorf("%w: dist-tag may not begin with '.': %q", core.ErrInvalidName, tag)
	}
	if tag == "." || tag == ".." || path.Clean(tag) != tag {
		return fmt.Errorf("%w: path traversal is not allowed: %q", core.ErrInvalidName, tag)
	}
	if strings.ContainsAny(tag, "/\\ \t\r\n") {
		return fmt.Errorf("%w: illegal character in dist-tag %q", core.ErrInvalidName, tag)
	}
	return nil
}

// ValidateTarballName checks an attachment / download filename.
func ValidateTarballName(filename string) error {
	if filename == "" {
		return fmt.Errorf("%w: empty tarball name", core.ErrInvalidName)
	}
	if strings.HasPrefix(filename, ".") {
		return fmt.Errorf("%w: leading dot is reserved: %q", core.ErrInvalidName, filename)
	}
	if path.Clean(filename) != filename || strings.ContainsAny(filename, "/\\ \t\r\n") {
		return fmt.Errorf("%w: illegal tarball name %q", core.ErrInvalidName, filename)
	}
	if !strings.HasSuffix(filename, ".tgz") {
		return fmt.Errorf("%w: tarball name must end in .tgz: %q", core.ErrInvalidName, filename)
	}
	return nil
}

// errBadDigest marks a malformed publisher-declared digest (bad hex shasum or
// SRI), distinct from a digest that is well-formed but does not match the bytes
// (core.ErrDigestMismatch, raised by the Store during the streamed write).
var errBadDigest = errors.New("npm: malformed declared digest")

// expectedDigests translates a version's dist.shasum (hex SHA-1) and
// dist.integrity ("sha512-<base64>") into core.ExpectedDigest checks the Store
// verifies while streaming the tarball. Absent values are skipped; an integrity
// hash other than sha512 is ignored (npm always uses sha512). A malformed
// declared value returns errBadDigest.
func expectedDigests(dist map[string]any) ([]core.ExpectedDigest, error) {
	if dist == nil {
		return nil, nil
	}
	var out []core.ExpectedDigest
	if s, _ := dist["shasum"].(string); strings.TrimSpace(s) != "" {
		raw, err := hex.DecodeString(strings.TrimSpace(s))
		if err != nil {
			return nil, fmt.Errorf("%w: shasum %q", errBadDigest, s)
		}
		out = append(out, core.ExpectedDigest{Hash: crypto.SHA1, Sum: raw})
	}
	if s, _ := dist["integrity"].(string); strings.TrimSpace(s) != "" {
		s = strings.TrimSpace(s)
		if rest, ok := strings.CutPrefix(s, "sha512-"); ok {
			raw, err := base64.StdEncoding.DecodeString(rest)
			if err != nil {
				return nil, fmt.Errorf("%w: integrity %q", errBadDigest, s)
			}
			out = append(out, core.ExpectedDigest{Hash: crypto.SHA512, Sum: raw})
		}
	}
	return out, nil
}
