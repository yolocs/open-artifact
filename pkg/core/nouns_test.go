package core_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/yolocs/open-artifact/pkg/core"
)

func TestBuilderComposition(t *testing.T) {
	t.Parallel()

	t.Run("file", func(t *testing.T) {
		t.Parallel()

		got := core.Package{Name: "requests"}.
			Version("2.31.0").
			File("requests-2.31.0-py3-none-any.whl")

		want := core.File{
			Version: core.Version{
				Package: core.Package{Name: "requests"},
				Name:    "2.31.0",
			},
			Name: "requests-2.31.0-py3-none-any.whl",
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("File builder mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("version", func(t *testing.T) {
		t.Parallel()

		got := core.Package{Name: "requests"}.Version("2.31.0")
		want := core.Version{Package: core.Package{Name: "requests"}, Name: "2.31.0"}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("Version builder mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("tag from package", func(t *testing.T) {
		t.Parallel()

		got := core.Package{Name: "requests"}.Tag("latest", "2.31.0")
		want := core.Tag{
			Package: core.Package{Name: "requests"},
			Name:    "latest",
			Target:  "2.31.0",
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("Tag builder mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("tag from version", func(t *testing.T) {
		t.Parallel()

		got := core.Package{Name: "requests"}.Version("2.31.0").Tag("latest")
		want := core.Tag{
			Package: core.Package{Name: "requests"},
			Name:    "latest",
			Target:  "2.31.0",
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("Tag builder mismatch (-want +got):\n%s", diff)
		}
	})
}
