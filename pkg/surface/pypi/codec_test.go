package pypi

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestNormalizeProject(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "already normalized", in: "requests", want: "requests"},
		{name: "pep 503 separators collapse", in: "Foo_Bar.baz", want: "foo-bar-baz"},
		{name: "empty rejected", in: "", wantErr: true},
		{name: "leading dot rejected", in: ".hidden", wantErr: true},
		{name: "slash rejected", in: "a/b", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := NormalizeProject(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("NormalizeProject(%q) = nil error, want error", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeProject(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("NormalizeProject(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestValidateVersionAndFilename(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		version string
		file    string
		wantErr bool
	}{
		{name: "wheel ok", version: "1.0.0", file: "demo-1.0.0-py3-none-any.whl"},
		{name: "sdist tar ok", version: "1.0.0", file: "demo-1.0.0.tar.gz"},
		{name: "zip ok", version: "1.0.0", file: "demo-1.0.0.zip"},
		{name: "dot version rejected", version: ".1.0.0", file: "demo-1.0.0.whl", wantErr: true},
		{name: "traversal version rejected", version: "../1", file: "demo-1.0.0.whl", wantErr: true},
		{name: "dot file rejected", version: "1.0.0", file: ".demo.whl", wantErr: true},
		{name: "slash file rejected", version: "1.0.0", file: "dist/demo.whl", wantErr: true},
		{name: "unknown extension rejected", version: "1.0.0", file: "demo-1.0.0.txt", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateVersion(tc.version)
			if err == nil {
				err = ValidateFilename(tc.file)
			}
			if tc.wantErr != (err != nil) {
				t.Fatalf("validation err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func TestWantsJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		accept string
		want   bool
	}{
		{name: "absent defaults html", accept: "", want: false},
		{name: "json explicit", accept: "application/vnd.pypi.simple.v1+json", want: true},
		{name: "json beats html", accept: "text/html;q=0.2, application/vnd.pypi.simple.v1+json;q=0.8", want: true},
		{name: "html beats json", accept: "text/html;q=0.9, application/vnd.pypi.simple.v1+json;q=0.1", want: false},
		{name: "equal prefers json", accept: "text/html;q=0.5, application/vnd.pypi.simple.v1+json;q=0.5", want: true},
		{name: "json q zero ignored", accept: "application/vnd.pypi.simple.v1+json;q=0", want: false},
		{name: "unparseable defaults html", accept: "not a real accept header", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := WantsJSON(tc.accept); got != tc.want {
				t.Fatalf("WantsJSON(%q) = %v, want %v", tc.accept, got, tc.want)
			}
		})
	}
}

func TestHashFromDigest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		digest string
		want   string
	}{
		{name: "sha256", digest: "sha256:abc123", want: "abc123"},
		{name: "other digest omitted", digest: "sha512:abc123", want: ""},
		{name: "missing prefix omitted", digest: "abc123", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := HashFromDigest(tc.digest); got != tc.want {
				t.Fatalf("HashFromDigest(%q) = %q, want %q", tc.digest, got, tc.want)
			}
		})
	}
}

func TestRenderRootHTML(t *testing.T) {
	t.Parallel()

	got := RenderRootHTML([]Project{{Name: "zeta"}, {Name: "alpha"}})
	want := `<!doctype html>
<html>
<body>
<a href="alpha/">alpha</a>
<a href="zeta/">zeta</a>
</body>
</html>
`
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("RenderRootHTML mismatch (-want +got):\n%s", diff)
	}
}

func TestRenderProjectHTML(t *testing.T) {
	t.Parallel()

	got := RenderProjectHTML(ProjectPage{
		Name: "demo",
		Files: []FileLink{{
			Filename:       "demo-1.0.0-py3-none-any.whl",
			URL:            "/team/simple/../packages/demo/1.0.0/demo-1.0.0-py3-none-any.whl",
			SHA256:         "abc123",
			RequiresPython: ">=3.11",
		}},
	})
	want := `<!doctype html>
<html>
<body>
<a href="/team/simple/../packages/demo/1.0.0/demo-1.0.0-py3-none-any.whl#sha256=abc123" data-requires-python="&gt;=3.11">demo-1.0.0-py3-none-any.whl</a>
</body>
</html>
`
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("RenderProjectHTML mismatch (-want +got):\n%s", diff)
	}
}

func TestProjectJSON(t *testing.T) {
	t.Parallel()

	got := ProjectJSON(ProjectPage{
		Name: "demo",
		Files: []FileLink{{
			Filename:       "demo-1.0.0-py3-none-any.whl",
			URL:            "/team/packages/demo/1.0.0/demo-1.0.0-py3-none-any.whl",
			SHA256:         "abc123",
			RequiresPython: ">=3.11",
		}},
	})
	want := SimpleJSON{
		Meta: MetaJSON{APIVersion: "1.0"},
		Name: "demo",
		Files: []FileJSON{{
			Filename:       "demo-1.0.0-py3-none-any.whl",
			URL:            "/team/packages/demo/1.0.0/demo-1.0.0-py3-none-any.whl",
			Hashes:         map[string]string{"sha256": "abc123"},
			RequiresPython: ">=3.11",
		}},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("ProjectJSON mismatch (-want +got):\n%s", diff)
	}
}
