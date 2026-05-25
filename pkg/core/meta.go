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
type Meta struct {
	Digest      string         `json:"digest,omitempty"`
	Size        int64          `json:"size,omitempty"`
	CreatedAt   time.Time      `json:"createdAt,omitempty"`
	UpdatedAt   time.Time      `json:"updatedAt,omitempty"`
	Annotations map[string]any `json:"annotations,omitempty"`
}
