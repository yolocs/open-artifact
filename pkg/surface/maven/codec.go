package maven

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net/url"
	"regexp"
	"strings"

	"github.com/yolocs/open-artifact/pkg/core"
	"github.com/yolocs/open-artifact/pkg/namespace"
)

const (
	mavenFormat = string(core.FormatMaven)

	metadataFile  = "maven-metadata.xml"
	archetypeFile = "archetype-catalog.xml"
)

var segmentPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

type pathKind int

const (
	pathArtifact pathKind = iota
	pathArtifactMetadata
	pathVersionMetadata
	pathArchetypeCatalog
)

type checksumAlgorithm string

const (
	checksumNone   checksumAlgorithm = ""
	checksumMD5    checksumAlgorithm = "md5"
	checksumSHA1   checksumAlgorithm = "sha1"
	checksumSHA256 checksumAlgorithm = "sha256"
	checksumSHA512 checksumAlgorithm = "sha512"
)

type requestPath struct {
	Namespace  string
	Kind       pathKind
	GroupPath  []string
	GroupID    string
	ArtifactID string
	Package    string
	Version    string
	File       string
	Checksum   checksumAlgorithm
	TargetFile string
}

func parsePath(escapedPath string) (requestPath, error) {
	if !strings.HasPrefix(escapedPath, "/") {
		return requestPath{}, fmt.Errorf("%w: Maven path must be absolute", core.ErrInvalidName)
	}
	raw := strings.Split(strings.TrimPrefix(escapedPath, "/"), "/")
	if len(raw) < 3 {
		return requestPath{}, fmt.Errorf("%w: Maven path too short", core.ErrInvalidName)
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
	if root != "maven2" {
		return requestPath{}, fmt.Errorf("%w: missing maven2 root", core.ErrInvalidName)
	}
	segments, err := unescapeSegments(raw[2:])
	if err != nil {
		return requestPath{}, err
	}
	for _, seg := range segments {
		if err := validateMavenSegment(seg); err != nil {
			return requestPath{}, err
		}
	}

	if len(segments) == 1 && segments[0] == archetypeFile {
		return requestPath{
			Namespace: ns,
			Kind:      pathArchetypeCatalog,
			File:      archetypeFile,
		}, nil
	}
	if len(segments) < 3 {
		return requestPath{}, fmt.Errorf("%w: Maven coordinate path too short", core.ErrInvalidName)
	}

	file := segments[len(segments)-1]
	checksum, targetFile := checksumTarget(file)
	metadataTarget := targetFile == metadataFile || file == metadataFile
	if metadataTarget && len(segments) >= 4 && isSnapshotVersion(segments[len(segments)-2]) {
		return buildCoordinatePath(ns, pathVersionMetadata, segments[:len(segments)-3], segments[len(segments)-3], segments[len(segments)-2], file, checksum, targetFile)
	}
	if metadataTarget {
		return buildCoordinatePath(ns, pathArtifactMetadata, segments[:len(segments)-2], segments[len(segments)-2], "", file, checksum, targetFile)
	}
	if len(segments) < 4 {
		return requestPath{}, fmt.Errorf("%w: Maven artifact path too short", core.ErrInvalidName)
	}
	return buildCoordinatePath(ns, pathArtifact, segments[:len(segments)-3], segments[len(segments)-3], segments[len(segments)-2], file, checksum, targetFile)
}

func buildCoordinatePath(ns string, kind pathKind, group []string, artifactID, version, file string, checksum checksumAlgorithm, targetFile string) (requestPath, error) {
	if len(group) == 0 {
		return requestPath{}, fmt.Errorf("%w: Maven group path is required", core.ErrInvalidName)
	}
	packageName := strings.Join(append(append([]string{}, group...), artifactID), "/")
	return requestPath{
		Namespace:  ns,
		Kind:       kind,
		GroupPath:  append([]string{}, group...),
		GroupID:    strings.Join(group, "."),
		ArtifactID: artifactID,
		Package:    packageName,
		Version:    version,
		File:       file,
		Checksum:   checksum,
		TargetFile: targetFile,
	}, nil
}

func unescapeSegments(raw []string) ([]string, error) {
	out := make([]string, 0, len(raw))
	for _, seg := range raw {
		decoded, err := unescapeSegment(seg)
		if err != nil {
			return nil, err
		}
		out = append(out, decoded)
	}
	return out, nil
}

func unescapeSegment(seg string) (string, error) {
	decoded, err := url.PathUnescape(seg)
	if err != nil {
		return "", fmt.Errorf("%w: malformed Maven path escape", core.ErrInvalidName)
	}
	return decoded, nil
}

func validateMavenSegment(seg string) error {
	switch {
	case seg == "":
		return fmt.Errorf("%w: empty Maven path segment", core.ErrInvalidName)
	case seg == "." || seg == "..":
		return fmt.Errorf("%w: path traversal is not allowed: %q", core.ErrInvalidName, seg)
	case strings.HasPrefix(seg, "."):
		return fmt.Errorf("%w: leading dot is reserved: %q", core.ErrInvalidName, seg)
	case !segmentPattern.MatchString(seg):
		return fmt.Errorf("%w: invalid Maven path segment: %q", core.ErrInvalidName, seg)
	default:
		return nil
	}
}

func checksumTarget(filename string) (checksumAlgorithm, string) {
	for suffix, alg := range map[string]checksumAlgorithm{
		".md5":    checksumMD5,
		".sha1":   checksumSHA1,
		".sha256": checksumSHA256,
		".sha512": checksumSHA512,
	} {
		if strings.HasSuffix(filename, suffix) {
			return alg, strings.TrimSuffix(filename, suffix)
		}
	}
	return checksumNone, ""
}

func isSnapshotVersion(version string) bool {
	return strings.HasSuffix(strings.ToUpper(version), "-SNAPSHOT")
}

func verifyChecksum(alg checksumAlgorithm, target io.Reader, declared io.Reader) error {
	h, err := hashForChecksum(alg)
	if err != nil {
		return err
	}
	if _, err := io.Copy(h, target); err != nil {
		return err
	}
	body, err := io.ReadAll(declared)
	if err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	want := strings.Fields(string(body))
	if len(want) == 0 || !strings.EqualFold(got, want[0]) {
		return core.ErrDigestMismatch
	}
	return nil
}

func hashForChecksum(alg checksumAlgorithm) (hash.Hash, error) {
	switch alg {
	case checksumMD5:
		return md5.New(), nil
	case checksumSHA1:
		return sha1.New(), nil
	case checksumSHA256:
		return sha256.New(), nil
	case checksumSHA512:
		return sha512.New(), nil
	default:
		return nil, fmt.Errorf("%w: unsupported checksum algorithm %q", core.ErrInvalidName, alg)
	}
}
