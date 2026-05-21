package pypi

import (
	"encoding/json"
	"html"
	"mime"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// Simple-index media types (PEP 503 / PEP 691).
const (
	contentTypeJSONv1 = "application/vnd.pypi.simple.v1+json"
	contentTypeHTMLv1 = "application/vnd.pypi.simple.v1+html"
	contentTypeHTML   = "text/html"

	// apiVersion advertises the PEP 691 schema version served. We stay at 1.0
	// because the optional PEP 700 fields (versions/size/upload-time) are not
	// emitted; bumping without them would mis-signal capability.
	apiVersion = "1.0"
)

// indexFile is one downloadable distribution in a project index.
type indexFile struct {
	Filename string
	// URL is the absolute path (no scheme/host) pip fetches to download the
	// file, served by this surface so downloads route through the cache.
	URL string
	// Sha256 is the hex digest (no "sha256:" prefix), empty when unknown.
	Sha256 string
}

// pickContentType chooses the simple-index representation from an Accept
// header. JSON is selected only when application/vnd.pypi.simple.v1+json
// appears with a q-value at least as high as any HTML alternative; otherwise
// HTML is served (the default for absent or unparseable Accept), keeping
// pre-PEP-691 clients working.
func pickContentType(accept string) string {
	if accept == "" {
		return contentTypeHTML
	}
	jsonQ, htmlQ := -1.0, -1.0
	for _, raw := range strings.Split(accept, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		mt, params, err := mime.ParseMediaType(raw)
		if err != nil {
			continue
		}
		q := 1.0
		if v := params["q"]; v != "" {
			if f, perr := strconv.ParseFloat(v, 64); perr == nil {
				q = f
			}
		}
		if q == 0 {
			continue
		}
		switch mt {
		case contentTypeJSONv1:
			if q > jsonQ {
				jsonQ = q
			}
		case contentTypeHTMLv1, contentTypeHTML:
			if q > htmlQ {
				htmlQ = q
			}
		}
	}
	if jsonQ < 0 {
		return contentTypeHTML
	}
	if htmlQ < 0 || jsonQ >= htmlQ {
		return contentTypeJSONv1
	}
	return contentTypeHTML
}

// jsonProjectFile is the PEP 691 per-file shape.
type jsonProjectFile struct {
	Filename string            `json:"filename"`
	URL      string            `json:"url"`
	Hashes   map[string]string `json:"hashes"`
}

type jsonMeta struct {
	APIVersion string `json:"api-version"`
}

type jsonProject struct {
	Meta  jsonMeta          `json:"meta"`
	Name  string            `json:"name"`
	Files []jsonProjectFile `json:"files"`
}

type jsonListProject struct {
	Name string `json:"name"`
}

type jsonProjectList struct {
	Meta     jsonMeta          `json:"meta"`
	Projects []jsonListProject `json:"projects"`
}

// writeProjectIndex renders a single project's file list as HTML or JSON,
// chosen from the request's Accept header. files are rendered in the order
// given; callers sort beforehand for stable output.
func writeProjectIndex(w http.ResponseWriter, accept, name string, files []indexFile) {
	if pickContentType(accept) == contentTypeJSONv1 {
		out := jsonProject{
			Meta:  jsonMeta{APIVersion: apiVersion},
			Name:  name,
			Files: make([]jsonProjectFile, 0, len(files)),
		}
		for _, f := range files {
			jf := jsonProjectFile{Filename: f.Filename, URL: f.URL, Hashes: map[string]string{}}
			if f.Sha256 != "" {
				jf.Hashes["sha256"] = f.Sha256
			}
			out.Files = append(out.Files, jf)
		}
		writeJSON(w, contentTypeJSONv1, out)
		return
	}

	var b strings.Builder
	b.WriteString("<!DOCTYPE html>\n<html><head><title>Links for ")
	b.WriteString(html.EscapeString(name))
	b.WriteString("</title></head><body>\n<h1>Links for ")
	b.WriteString(html.EscapeString(name))
	b.WriteString("</h1>\n")
	for _, f := range files {
		href := f.URL
		if f.Sha256 != "" {
			href += "#sha256=" + f.Sha256
		}
		b.WriteString(`<a href="`)
		b.WriteString(html.EscapeString(href))
		b.WriteString(`">`)
		b.WriteString(html.EscapeString(f.Filename))
		b.WriteString("</a><br/>\n")
	}
	b.WriteString("</body></html>\n")
	writeHTML(w, b.String())
}

// writeProjectList renders the root index — the set of known project names —
// as HTML or JSON. names are rendered in the order given.
func writeProjectList(w http.ResponseWriter, accept, prefix string, names []string) {
	if pickContentType(accept) == contentTypeJSONv1 {
		out := jsonProjectList{
			Meta:     jsonMeta{APIVersion: apiVersion},
			Projects: make([]jsonListProject, 0, len(names)),
		}
		for _, n := range names {
			out.Projects = append(out.Projects, jsonListProject{Name: n})
		}
		writeJSON(w, contentTypeJSONv1, out)
		return
	}

	var b strings.Builder
	b.WriteString("<!DOCTYPE html>\n<html><head><title>Simple Index</title></head><body>\n")
	for _, n := range names {
		b.WriteString(`<a href="`)
		b.WriteString(html.EscapeString(prefix + "/simple/" + n + "/"))
		b.WriteString(`">`)
		b.WriteString(html.EscapeString(n))
		b.WriteString("</a><br/>\n")
	}
	b.WriteString("</body></html>\n")
	writeHTML(w, b.String())
}

func writeJSON(w http.ResponseWriter, contentType string, v any) {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}

func writeHTML(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", contentTypeHTML)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

// sortIndexFiles orders files by filename for deterministic output.
func sortIndexFiles(files []indexFile) {
	sort.Slice(files, func(i, j int) bool { return files[i].Filename < files[j].Filename })
}
