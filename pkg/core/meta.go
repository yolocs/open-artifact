package core

import "time"

// Meta is the baseline envelope persisted alongside artifact records. The
// Store interprets only the baseline fields; Ext is an opaque, caller-owned
// payload the Store round-trips verbatim and never reads.
//
// Size is intentionally absent — derive it from the bucket's object
// attributes rather than trusting a stored value.
type Meta struct {
	Digest    string         `json:"digest,omitempty"`
	CreatedAt time.Time      `json:"createdAt,omitempty"`
	UpdatedAt time.Time      `json:"updatedAt,omitempty"`
	Ext       map[string]any `json:"ext,omitempty"`
}
