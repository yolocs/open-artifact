package pypi

import (
	"fmt"
	"html"
	"mime"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/yolocs/open-artifact/pkg/core"
)

const simpleJSONMediaType = "application/vnd.pypi.simple.v1+json"

var pep503Separators = regexp.MustCompile(`[-_.]+`)

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

func WantsJSON(accept string) bool {
	if strings.TrimSpace(accept) == "" {
		return false
	}
	bestJSON := -1.0
	bestHTML := -1.0
	for _, part := range strings.Split(accept, ",") {
		mt, params, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil {
			return false
		}
		q := 1.0
		if raw := params["q"]; raw != "" {
			parsed, err := strconv.ParseFloat(raw, 64)
			if err != nil || parsed < 0 || parsed > 1 {
				return false
			}
			q = parsed
		}
		switch mt {
		case simpleJSONMediaType:
			if q > bestJSON {
				bestJSON = q
			}
		case "text/html", "application/vnd.pypi.simple.v1+html":
			if q > bestHTML {
				bestHTML = q
			}
		}
	}
	return bestJSON > 0 && bestJSON >= bestHTML
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
	b.WriteString("<!doctype html>\n<html>\n<body>\n")
	for _, p := range projects {
		name := html.EscapeString(p.Name)
		b.WriteString(`<a href="`)
		b.WriteString(name)
		b.WriteString(`/">`)
		b.WriteString(name)
		b.WriteString("</a>\n")
	}
	b.WriteString("</body>\n</html>\n")
	return b.String()
}

func RenderProjectHTML(page ProjectPage) string {
	files := append([]FileLink(nil), page.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Filename < files[j].Filename })
	var b strings.Builder
	b.WriteString("<!doctype html>\n<html>\n<body>\n")
	for _, f := range files {
		href := f.URL
		if f.SHA256 != "" {
			href += "#sha256=" + f.SHA256
		}
		b.WriteString(`<a href="`)
		b.WriteString(html.EscapeString(href))
		b.WriteString(`"`)
		if f.RequiresPython != "" {
			b.WriteString(` data-requires-python="`)
			b.WriteString(html.EscapeString(f.RequiresPython))
			b.WriteString(`"`)
		}
		b.WriteString(">")
		b.WriteString(html.EscapeString(f.Filename))
		b.WriteString("</a>\n")
	}
	b.WriteString("</body>\n</html>\n")
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
