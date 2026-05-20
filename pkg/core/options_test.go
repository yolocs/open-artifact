package core_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/yolocs/open-artifact/pkg/core"
)

func TestNewCreateConfig(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		opts []core.CreateOption
		want core.CreateConfig
	}{
		{
			name: "no options",
			want: core.CreateConfig{},
		},
		{
			name: "single annotations",
			opts: []core.CreateOption{
				core.WithAnnotations(map[string]any{"yanked": false, "score": 9.5}),
			},
			want: core.CreateConfig{Annotations: map[string]any{"yanked": false, "score": 9.5}},
		},
		{
			name: "empty annotations is a no-op",
			opts: []core.CreateOption{core.WithAnnotations(map[string]any{})},
			want: core.CreateConfig{},
		},
		{
			name: "repeated options merge, later keys win",
			opts: []core.CreateOption{
				core.WithAnnotations(map[string]any{"a": 1, "b": 2}),
				core.WithAnnotations(map[string]any{"b": 3, "c": 4}),
			},
			want: core.CreateConfig{Annotations: map[string]any{"a": 1, "b": 3, "c": 4}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := core.NewCreateConfig(tc.opts...)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("NewCreateConfig mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestWithAnnotationsDoesNotAliasCallerMap(t *testing.T) {
	t.Parallel()

	src := map[string]any{"k": "v"}
	cfg := core.NewCreateConfig(core.WithAnnotations(src))

	src["k"] = "mutated"
	if got := cfg.Annotations["k"]; got != "v" {
		t.Errorf("config aliased caller map: got %v, want v", got)
	}
}
