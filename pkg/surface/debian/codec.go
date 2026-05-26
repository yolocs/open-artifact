package debian

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/yolocs/open-artifact/pkg/core"
	"github.com/yolocs/open-artifact/pkg/namespace"
)

const debianFormat = string(core.FormatDebian)

// debianRoot is the fixed path segment after the namespace: /{ns}/debian/...
const debianRoot = "debian"

type pathKind int

const (
	// pathIndex is anything under dists/ — repository metadata served verbatim.
	pathIndex pathKind = iota
	// pathPool is anything under pool/ — an artifact file (.deb/.dsc/.tar.*).
	pathPool
)

// sourceArchiveSuffixes are the multi-dot extensions of non-.deb pool files,
// stripped (longest first) to isolate the version when parsing a source
// filename for the filter Ref. Order matters: ".orig.tar.gz" must precede
// ".tar.gz".
var sourceArchiveSuffixes = []string{
	".orig.tar.gz", ".orig.tar.xz", ".orig.tar.bz2", ".orig.tar.zst",
	".debian.tar.gz", ".debian.tar.xz", ".debian.tar.bz2", ".debian.tar.zst",
	".tar.gz", ".tar.xz", ".tar.bz2", ".tar.zst",
	".diff.gz", ".dsc", ".changes", ".buildinfo",
}

type requestPath struct {
	Namespace string
	Kind      pathKind

	// RestRaw is the still-escaped repo-relative path (everything after
	// /{ns}/debian/). It is used to build the upstream URL byte-identically and
	// as the logical cache/negative-cache key.
	RestRaw string

	// Pool fields (Kind == pathPool only).
	PoolDir string // decoded directory under pool/, e.g. "main/h/hello"
	File    string // decoded filename, e.g. "hello_2.10-2_amd64.deb"
	PkgName string // best-effort package name parsed from File (filter Ref)
	Version string // best-effort version parsed from File (filter Ref)
}

// parsePath parses an escaped request path of the form
// /{namespace}/debian/<repo-path> into a requestPath. It validates the
// namespace and every decoded segment (rejecting empty/traversal/leading-dot
// and decoded path separators), but performs no I/O.
func parsePath(escapedPath string) (requestPath, error) {
	if !strings.HasPrefix(escapedPath, "/") {
		return requestPath{}, fmt.Errorf("%w: Debian path must be absolute", core.ErrInvalidName)
	}
	raw := strings.Split(strings.TrimPrefix(escapedPath, "/"), "/")
	if len(raw) < 3 {
		return requestPath{}, fmt.Errorf("%w: Debian path too short", core.ErrInvalidName)
	}
	ns, err := unescapeSegment(raw[0])
	if err != nil {
		return requestPath{}, err
	}
	if err := namespace.ValidateName(ns); err != nil {
		return requestPath{}, err
	}
	root, err := unescapeSegment(raw[1])
	if err != nil {
		return requestPath{}, err
	}
	if root != debianRoot {
		return requestPath{}, fmt.Errorf("%w: missing debian root", core.ErrInvalidName)
	}

	rest := raw[2:]
	decoded := make([]string, len(rest))
	for i, seg := range rest {
		d, err := unescapeSegment(seg)
		if err != nil {
			return requestPath{}, err
		}
		if err := validateSegment(d); err != nil {
			return requestPath{}, err
		}
		decoded[i] = d
	}
	restRaw := strings.Join(rest, "/")

	switch decoded[0] {
	case "pool":
		if len(decoded) < 2 {
			return requestPath{}, fmt.Errorf("%w: Debian pool path too short", core.ErrInvalidName)
		}
		file := decoded[len(decoded)-1]
		poolDir := strings.Join(decoded[1:len(decoded)-1], "/")
		pkg, version := parsePoolFile(file)
		if poolDir == "" {
			// A file directly under pool/ is unusual; fall back to the parsed
			// package name so the Store location stays deterministic.
			poolDir = pkg
		}
		return requestPath{
			Namespace: ns,
			Kind:      pathPool,
			RestRaw:   restRaw,
			PoolDir:   poolDir,
			File:      file,
			PkgName:   pkg,
			Version:   version,
		}, nil
	case "dists":
		return requestPath{
			Namespace: ns,
			Kind:      pathIndex,
			RestRaw:   restRaw,
		}, nil
	default:
		// APT only fetches dists/ and pool/; anything else is not part of the
		// repository layout we proxy.
		return requestPath{}, fmt.Errorf("%w: unsupported Debian path root %q", core.ErrNotFound, decoded[0])
	}
}

func unescapeSegment(seg string) (string, error) {
	decoded, err := url.PathUnescape(seg)
	if err != nil {
		return "", fmt.Errorf("%w: malformed Debian path escape", core.ErrInvalidName)
	}
	return decoded, nil
}

func validateSegment(seg string) error {
	switch {
	case seg == "":
		return fmt.Errorf("%w: empty Debian path segment", core.ErrInvalidName)
	case seg == "." || seg == "..":
		return fmt.Errorf("%w: path traversal is not allowed: %q", core.ErrInvalidName, seg)
	case strings.HasPrefix(seg, "."):
		return fmt.Errorf("%w: leading dot is reserved: %q", core.ErrInvalidName, seg)
	case strings.Contains(seg, "/"):
		return fmt.Errorf("%w: encoded path separator is not allowed: %q", core.ErrInvalidName, seg)
	default:
		return nil
	}
}

// parsePoolFile extracts a best-effort package name and version from a pool
// filename for the filter Ref. Debian versions never contain '_', so splitting
// on '_' is safe. Parsing only feeds allow/deny/delay filter matching; the
// Store location is derived from the pool path, so an imperfect parse never
// causes a collision or a wrong cache entry.
//
//   - <name>_<version>_<arch>.deb / .udeb  -> name, version
//   - <name>_<version>.<archive-ext>       -> name, version (ext stripped)
func parsePoolFile(file string) (pkg, version string) {
	i := strings.IndexByte(file, '_')
	if i <= 0 {
		return file, ""
	}
	pkg = file[:i]
	rest := file[i+1:]

	if strings.HasSuffix(file, ".deb") || strings.HasSuffix(file, ".udeb") {
		if j := strings.IndexByte(rest, '_'); j > 0 {
			return pkg, rest[:j]
		}
		rest = strings.TrimSuffix(rest, ".udeb")
		rest = strings.TrimSuffix(rest, ".deb")
		return pkg, rest
	}

	for _, suffix := range sourceArchiveSuffixes {
		if strings.HasSuffix(rest, suffix) {
			return pkg, strings.TrimSuffix(rest, suffix)
		}
	}
	return pkg, rest
}

// indexContentType derives a stable Content-Type for an index file from its
// extension. APT does not rely on it, but a deterministic value keeps the
// fresh-fetch and stale-fallback responses identical.
func indexContentType(restPath string) string {
	switch {
	case strings.HasSuffix(restPath, ".gz"):
		return "application/gzip"
	case strings.HasSuffix(restPath, ".xz"):
		return "application/x-xz"
	case strings.HasSuffix(restPath, ".bz2"):
		return "application/x-bzip2"
	case strings.HasSuffix(restPath, ".zst"):
		return "application/zstd"
	case strings.HasSuffix(restPath, ".gpg"):
		return "application/pgp-signature"
	default:
		// Release, InRelease, Packages, Sources, Contents, etc.
		return "text/plain; charset=utf-8"
	}
}

// poolContentType picks the served Content-Type for a pool artifact.
func poolContentType(file string) string {
	if strings.HasSuffix(file, ".deb") || strings.HasSuffix(file, ".udeb") {
		return "application/vnd.debian.binary-package"
	}
	return "application/octet-stream"
}

func indexNegKey(p requestPath) string   { return "debian:index:" + p.RestRaw }
func poolNegKey(p requestPath) string    { return "debian:pool:" + p.PoolDir + "/" + p.File }
func indexCacheKey(p requestPath) string { return "debian:index:" + p.RestRaw }
