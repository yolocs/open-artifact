package pypi

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestNormalize(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"requests", "requests"},
		{"Foo_Bar", "foo-bar"},
		{"foo.bar", "foo-bar"},
		{"foo--bar", "foo-bar"},
		{"Django", "django"},
		{"zope.interface", "zope-interface"},
		{"a_-_.b", "a-b"},
		{"PyYAML", "pyyaml"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			if got := normalize(tc.in); got != tc.want {
				t.Errorf("normalize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestValidatePackageName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "simple", in: "requests", want: "requests"},
		{name: "normalizes", in: "Foo_Bar", want: "foo-bar"},
		{name: "dotted", in: "zope.interface", want: "zope-interface"},
		{name: "leading dot rejected", in: ".hidden", wantErr: true},
		{name: "leading dot after norm chars", in: ".foo", wantErr: true},
		{name: "empty rejected", in: "", wantErr: true},
		{name: "slash rejected", in: "foo/bar", wantErr: true},
		{name: "space rejected", in: "foo bar", wantErr: true},
		{name: "trailing separator rejected", in: "foo-", wantErr: true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := validatePackageName(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("validatePackageName(%q) = %q, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("validatePackageName(%q): unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("validatePackageName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestValidateVersion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		wantErr bool
	}{
		{"2.31.0", false},
		{"1.0.0a1", false},
		{"2024.1.1", false},
		{"1.0+local", false},
		{"1!2.0", false},
		{".hidden", true},
		{"", true},
		{"1.0/2", true},
		{"1 0", true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			err := validateVersion(tc.in)
			if tc.wantErr != (err != nil) {
				t.Errorf("validateVersion(%q) err=%v, wantErr=%v", tc.in, err, tc.wantErr)
			}
		})
	}
}

func TestValidateFilename(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		wantErr bool
	}{
		{"requests-2.31.0-py3-none-any.whl", false},
		{"requests-2.31.0.tar.gz", false},
		{"requests-2.31.0-py3-none-any.whl.metadata", false},
		{".meta", true},
		{".meta.requests-2.31.0.tar.gz", true},
		{"..", true},
		{".", true},
		{"foo/bar.whl", true},
		{"foo\\bar.whl", true},
		{"", true},
		{"with space.whl", true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			err := validateFilename(tc.in)
			if tc.wantErr != (err != nil) {
				t.Errorf("validateFilename(%q) err=%v, wantErr=%v", tc.in, err, tc.wantErr)
			}
		})
	}
}

func TestParseFilenameVersion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		filename string
		pkg      string
		want     string
		wantErr  bool
	}{
		{name: "wheel", filename: "requests-2.31.0-py3-none-any.whl", pkg: "requests", want: "2.31.0"},
		{name: "wheel with build tag", filename: "foo-1.0-1-py3-none-any.whl", pkg: "foo", want: "1.0-1"},
		{name: "sdist tar.gz", filename: "requests-2.31.0.tar.gz", pkg: "requests", want: "2.31.0"},
		{name: "sdist zip", filename: "requests-2.31.0.zip", pkg: "requests", want: "2.31.0"},
		{name: "underscore in filename", filename: "foo_bar-1.0-py3-none-any.whl", pkg: "foo-bar", want: "1.0"},
		{name: "pep658 metadata", filename: "requests-2.31.0-py3-none-any.whl.metadata", pkg: "requests", want: "2.31.0"},
		{name: "case insensitive", filename: "Requests-2.31.0.tar.gz", pkg: "requests", want: "2.31.0"},
		{name: "mismatch", filename: "other-1.0.tar.gz", pkg: "requests", wantErr: true},
		{name: "no version", filename: "requests.whl", pkg: "requests", wantErr: true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseFilenameVersion(tc.filename, tc.pkg)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseFilenameVersion(%q,%q) = %q, want error", tc.filename, tc.pkg, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseFilenameVersion(%q,%q): unexpected error: %v", tc.filename, tc.pkg, err)
			}
			if got != tc.want {
				t.Errorf("parseFilenameVersion(%q,%q) = %q, want %q", tc.filename, tc.pkg, got, tc.want)
			}
		})
	}
}

func TestPickContentType(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		accept string
		want   string
	}{
		{name: "empty defaults html", accept: "", want: contentTypeHTML},
		{name: "json explicit", accept: contentTypeJSONv1, want: contentTypeJSONv1},
		{name: "html legacy", accept: "text/html", want: contentTypeHTML},
		{name: "json preferred by q", accept: "text/html;q=0.1, " + contentTypeJSONv1 + ";q=0.9", want: contentTypeJSONv1},
		{name: "html preferred by q", accept: contentTypeJSONv1 + ";q=0.1, text/html;q=0.9", want: contentTypeHTML},
		{name: "json equal q wins", accept: contentTypeJSONv1 + ";q=0.5, text/html;q=0.5", want: contentTypeJSONv1},
		{name: "unparseable defaults html", accept: "???", want: contentTypeHTML},
		{name: "wildcard ignored", accept: "*/*", want: contentTypeHTML},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := pickContentType(tc.accept); got != tc.want {
				t.Errorf("pickContentType(%q) = %q, want %q", tc.accept, got, tc.want)
			}
		})
	}
}

func TestSortIndexFiles(t *testing.T) {
	t.Parallel()
	files := []indexFile{
		{Filename: "z.whl"},
		{Filename: "a.whl"},
		{Filename: "m.tar.gz"},
	}
	sortIndexFiles(files)
	got := []string{files[0].Filename, files[1].Filename, files[2].Filename}
	want := []string{"a.whl", "m.tar.gz", "z.whl"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("sortIndexFiles mismatch (-want +got):\n%s", diff)
	}
}
