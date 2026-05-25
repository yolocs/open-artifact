package core

import "time"

// Meta is the baseline envelope persisted alongside artifact records. The
// Store interprets only the baseline fields; Annotations is an opaque,
// caller-owned payload the Store round-trips verbatim and never reads.
//
// Size is the byte length of the associated blob, recorded at write time (the
// Store counts the bytes it streams). It is trusted the same way the Digest is,
// so a single .meta read yields digest, size, and annotations without a
// separate bucket-attributes call. When the sidecar is absent the Store
// recomputes it from the bucket's object attributes.
//
// Digest is the canonical SHA-256 ("sha256:<hex>") and is the file's content
// identity. Digests carries any *additional* content hashes the caller asked
// the Store to compute and verify on write (via WithExpectedDigests), keyed by
// a short algorithm name ("sha1", "sha512", "md5") with a lowercase-hex value —
// so a format can serve npm integrity or Maven checksum sidecars without
// re-reading the blob. It excludes the canonical SHA-256 (that lives in Digest).
type Meta struct {
	Digest      string            `json:"digest,omitempty"`
	Digests     map[string]string `json:"digests,omitempty"`
	Size        int64             `json:"size,omitempty"`
	CreatedAt   time.Time         `json:"createdAt,omitempty"`
	UpdatedAt   time.Time         `json:"updatedAt,omitempty"`
	Annotations map[string]any    `json:"annotations,omitempty"`
}
