package core_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/yolocs/open-artifact/pkg/core"
)

func TestMetaJSONRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		meta core.Meta
	}{
		{
			name: "baseline only",
			meta: core.Meta{
				Digest:    "sha256:abc",
				CreatedAt: time.Date(2026, 5, 20, 1, 2, 3, 0, time.UTC),
				UpdatedAt: time.Date(2026, 5, 20, 4, 5, 6, 0, time.UTC),
			},
		},
		{
			name: "arbitrary annotations",
			meta: core.Meta{
				Digest:    "sha256:def",
				CreatedAt: time.Date(2026, 5, 20, 1, 2, 3, 0, time.UTC),
				UpdatedAt: time.Date(2026, 5, 20, 4, 5, 6, 0, time.UTC),
				// JSON numbers decode to float64; use float64 so the
				// round-trip is lossless without custom decoding.
				Annotations: map[string]any{
					"requires_python": ">=3.8",
					"yanked":          false,
					"downloads":       float64(12345),
					"nested": map[string]any{
						"classifiers": []any{"Programming Language :: Python", "License :: OSI"},
						"score":       float64(9.5),
					},
				},
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

			b, err := json.Marshal(tc.meta)
			if err != nil {
				t.Fatalf("json.Marshal: %v", err)
			}

			var got core.Meta
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatalf("json.Unmarshal: %v", err)
			}

			if diff := cmp.Diff(tc.meta, got); diff != "" {
				t.Errorf("Meta round-trip mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestMetaOmitsAnnotationsWhenEmpty(t *testing.T) {
	t.Parallel()

	b, err := json.Marshal(core.Meta{Digest: "sha256:abc"})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if _, ok := raw["annotations"]; ok {
		t.Errorf("expected annotations to be omitted, got: %s", b)
	}
}
