package generic

import (
	"crypto"
	_ "crypto/sha1"   // register SHA-1 for X-Checksum-Sha1 verification
	_ "crypto/sha256" // register SHA-256 for X-Checksum-Sha256 verification
	_ "crypto/sha512" // register SHA-512 for X-Checksum-Sha512 verification
	"encoding/hex"
	"fmt"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/yolocs/open-artifact/pkg/core"
)

const genericFormat = string(core.FormatGeneric)

// annContentType records a file's upload Content-Type so it can be served back
// verbatim on download.
const annContentType = "generic:content_type"

// createRequest is the optional JSON body accepted on PUT of a package or
// version. Annotations are stored verbatim on the record's metadata envelope and
// replace any previous annotations.
type createRequest struct {
	Annotations map[string]any `json:"annotations,omitempty"`
}

type packageListResponse struct {
	Namespace string   `json:"namespace"`
	Packages  []string `json:"packages"`
}

type packageResponse struct {
	Name        string         `json:"name"`
	CreatedAt   *time.Time     `json:"created_at,omitempty"`
	UpdatedAt   *time.Time     `json:"updated_at,omitempty"`
	Annotations map[string]any `json:"annotations,omitempty"`
	Versions    []string       `json:"versions"`
}

type versionListResponse struct {
	Namespace string   `json:"namespace"`
	Package   string   `json:"package"`
	Versions  []string `json:"versions"`
}

type versionResponse struct {
	Package     string         `json:"package"`
	Version     string         `json:"version"`
	CreatedAt   *time.Time     `json:"created_at,omitempty"`
	UpdatedAt   *time.Time     `json:"updated_at,omitempty"`
	Annotations map[string]any `json:"annotations,omitempty"`
	Files       []string       `json:"files"`
}

type fileResponse struct {
	Name        string     `json:"name"`
	Size        int64      `json:"size"`
	Digest      string     `json:"digest,omitempty"`
	ContentType string     `json:"content_type,omitempty"`
	CreatedAt   *time.Time `json:"created_at,omitempty"`
}

type fileListResponse struct {
	Namespace string         `json:"namespace"`
	Package   string         `json:"package"`
	Version   string         `json:"version"`
	Files     []fileResponse `json:"files"`
}

type uploadResponse struct {
	Package string `json:"package"`
	Version string `json:"version"`
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	Digest  string `json:"digest,omitempty"`
}

// names projects a slice of handles to their names via Name.
func names[T interface{ Name() string }](items []T) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Name())
	}
	return out
}

// fileToResponse renders a file's name and Meta into the listing DTO, exposing
// the stored Content-Type annotation.
func fileToResponse(name string, m core.Meta) fileResponse {
	return fileResponse{
		Name:        name,
		Size:        m.Size,
		Digest:      m.Digest,
		ContentType: contentTypeFromMeta(m),
		CreatedAt:   timePtr(m.CreatedAt),
	}
}

// downloadContentType resolves a download's Content-Type from the stored
// annotation, falling back to an extension guess and finally octet-stream.
func downloadContentType(name string, m core.Meta) string {
	if ct := contentTypeFromMeta(m); ct != "" {
		return ct
	}
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

func contentTypeFromMeta(m core.Meta) string {
	if ct, ok := m.Annotations[annContentType].(string); ok {
		return ct
	}
	return ""
}

// timePtr returns a pointer to t, or nil when t is the zero value, so JSON
// omitempty drops unset timestamps.
func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// checksumHeaders maps the optional integrity request headers to the hash the
// Store verifies the streamed body against.
var checksumHeaders = []struct {
	header string
	hash   crypto.Hash
}{
	{"X-Checksum-Sha256", crypto.SHA256},
	{"X-Checksum-Sha1", crypto.SHA1},
	{"X-Checksum-Sha512", crypto.SHA512},
}

// parseChecksumHeaders reads the optional X-Checksum-* integrity headers into
// ExpectedDigests the Store verifies during the streamed upload. A malformed
// (non-hex) value is a client error.
func parseChecksumHeaders(h http.Header) ([]core.ExpectedDigest, error) {
	var out []core.ExpectedDigest
	for _, c := range checksumHeaders {
		v := strings.TrimSpace(h.Get(c.header))
		if v == "" {
			continue
		}
		sum, err := hex.DecodeString(v)
		if err != nil {
			return nil, fmt.Errorf("invalid %s header: want lowercase hex", c.header)
		}
		out = append(out, core.ExpectedDigest{Hash: c.hash, Sum: sum})
	}
	return out, nil
}
