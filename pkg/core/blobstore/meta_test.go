package blobstore

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/yolocs/open-artifact/pkg/core"
)

func TestMetaCodecRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		meta core.Meta
	}{
		{
			name: "baseline",
			meta: core.Meta{
				Digest:    "sha256:abc",
				CreatedAt: time.Date(2026, 5, 20, 1, 2, 3, 0, time.UTC),
				UpdatedAt: time.Date(2026, 5, 20, 1, 2, 3, 0, time.UTC),
			},
		},
		{
			name: "with annotations",
			meta: core.Meta{
				Digest:      "sha256:def",
				CreatedAt:   time.Date(2026, 5, 20, 1, 2, 3, 0, time.UTC),
				UpdatedAt:   time.Date(2026, 5, 20, 1, 2, 3, 0, time.UTC),
				Annotations: map[string]any{"requires_python": ">=3.8"},
			},
		},
		{
			name: "empty",
			meta: core.Meta{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			b, err := encodeMeta(tc.meta)
			if err != nil {
				t.Fatalf("encodeMeta: %v", err)
			}
			got, err := decodeMeta(b)
			if err != nil {
				t.Fatalf("decodeMeta: %v", err)
			}
			if diff := cmp.Diff(tc.meta, got); diff != "" {
				t.Errorf("round-trip mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestDecodeMetaCorrupted(t *testing.T) {
	t.Parallel()

	if _, err := decodeMeta([]byte("{not json")); err == nil {
		t.Fatal("expected error decoding corrupted meta, got nil")
	}
}
