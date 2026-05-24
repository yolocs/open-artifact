package pypi

import (
	"fmt"
	"html/template"
	"mime"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/yolocs/open-artifact/pkg/core"
)

const simpleJSONMediaType = "application/vnd.pypi.simple.v1+json"

var pep503Separators = regexp.MustCompile(`[-_.]+`)

var (
	rootHTMLTemplate = template.Must(template.New("pypi-root").Parse(`<!doctype html>
<html>
<head>
<meta name="pypi:repository-version" content="1.0">
</head>
<body>
{{range .}}<a href="{{.Name}}/">{{.Name}}</a>
{{end}}</body>
</html>
`))
	projectHTMLTemplate = template.Must(template.New("pypi-project").Parse(`<!doctype html>
<html>
<head>
<meta name="pypi:repository-version" content="1.0">
</head>
<body>
{{range .Files}}<a href="{{.Href}}"{{if .RequiresPython}} data-requires-python="{{.RequiresPython}}"{{end}}>{{.Filename}}</a>
{{end}}</body>
</html>
`))
)

type Project struct {
	Name string
}

type ProjectPage struct {
	Name  string
	Files []FileLink
}

type FileLink struct {
	Filename       string
	URL            string
	SHA256         string
	RequiresPython string
}

func (f FileLink) Href() string {
	if f.SHA256 == "" {
		return f.URL
	}
	return f.URL + "#sha256=" + f.SHA256
}

type SimpleJSON struct {
	Meta  MetaJSON   `json:"meta"`
	Name  string     `json:"name"`
	Files []FileJSON `json:"files"`
}

type MetaJSON struct {
	APIVersion string `json:"api-version"`
}

type FileJSON struct {
	Filename       string            `json:"filename"`
	URL            string            `json:"url"`
	Hashes         map[string]string `json:"hashes,omitempty"`
	RequiresPython string            `json:"requires-python,omitempty"`
}

func NormalizeProject(name string) (string, error) {
	if err := validateSegment(name, true); err != nil {
		return "", err
	}
	return strings.ToLower(pep503Separators.ReplaceAllString(name, "-")), nil
}

func ValidateVersion(version string) error {
	return validateSegment(version, false)
}

func ValidateFilename(filename string) error {
	if err := validateSegment(filename, false); err != nil {
		return err
	}
	switch {
	case strings.HasSuffix(filename, ".whl"):
	case strings.HasSuffix(filename, ".tar.gz"):
	case strings.HasSuffix(filename, ".zip"):
	default:
		return fmt.Errorf("%w: unsupported PyPI file extension %q", core.ErrInvalidName, filename)
	}
	return nil
}

func validateSegment(s string, normalizeProject bool) error {
	if s == "" {
		return fmt.Errorf("%w: empty PyPI name", core.ErrInvalidName)
	}
	if strings.HasPrefix(s, ".") {
		return fmt.Errorf("%w: leading dot is reserved: %q", core.ErrInvalidName, s)
	}
	if strings.ContainsAny(s, `/\`) {
		return fmt.Errorf("%w: path separators are not allowed: %q", core.ErrInvalidName, s)
	}
	if !normalizeProject && (s == "." || s == ".." || path.Clean(s) != s) {
		return fmt.Errorf("%w: path traversal is not allowed: %q", core.ErrInvalidName, s)
	}
	return nil
}

func PrefersSimpleJSON(accept string) bool {
	return preferredSimpleResponse(accept) == simpleResponseJSON
}

type simpleResponse int

const (
	simpleResponseHTML simpleResponse = iota
	simpleResponseJSON
)

func preferredSimpleResponse(header string) simpleResponse {
	for _, part := range strings.Split(header, ",") {
		mt, params, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil {
			return simpleResponseHTML
		}
		if params["q"] == "0" {
			continue
		}
		switch mt {
		case simpleJSONMediaType, "*/*", "application/*":
			return simpleResponseJSON
		case "text/html", "application/vnd.pypi.simple.v1+html":
			return simpleResponseHTML
		}
	}
	return simpleResponseHTML
}

func HashFromDigest(digest string) string {
	if !strings.HasPrefix(digest, "sha256:") {
		return ""
	}
	return strings.TrimPrefix(digest, "sha256:")
}

func RenderRootHTML(projects []Project) string {
	sort.Slice(projects, func(i, j int) bool { return projects[i].Name < projects[j].Name })
	var b strings.Builder
	_ = rootHTMLTemplate.Execute(&b, projects)
	return b.String()
}

func RenderProjectHTML(page ProjectPage) string {
	files := append([]FileLink(nil), page.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Filename < files[j].Filename })
	var b strings.Builder
	_ = projectHTMLTemplate.Execute(&b, ProjectPage{Name: page.Name, Files: files})
	return b.String()
}

func ProjectJSON(page ProjectPage) SimpleJSON {
	files := append([]FileLink(nil), page.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Filename < files[j].Filename })
	out := SimpleJSON{
		Meta:  MetaJSON{APIVersion: "1.0"},
		Name:  page.Name,
		Files: make([]FileJSON, 0, len(files)),
	}
	for _, f := range files {
		jf := FileJSON{
			Filename:       f.Filename,
			URL:            f.URL,
			RequiresPython: f.RequiresPython,
		}
		if f.SHA256 != "" {
			jf.Hashes = map[string]string{"sha256": f.SHA256}
		}
		out.Files = append(out.Files, jf)
	}
	return out
}
