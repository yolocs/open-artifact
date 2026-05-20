package core

// The four data nouns of open-artifact compose upward
// (Package → Version → File, and Package/Version → Tag) as plain value
// structs. Builders chain so a fully-qualified File can be named in one
// expression:
//
//	file := core.Package{Name: "requests"}.
//		Version("2.31.0").
//		File("requests-2.31.0-py3-none-any.whl")

// Package is a named artifact in a scope.
type Package struct {
	Name string
}

// Version is a single release of a Package.
type Version struct {
	Package Package
	Name    string
}

// Tag is a named alias (a dist-tag) for a Package that points at a
// Version by name, e.g. "latest" → "2.31.0".
type Tag struct {
	Package Package
	Name    string
	Target  string
}

// File is a single uploadable blob within a Version.
//
// AllowOverwrite lets a caller opt in to replacing an existing file at the
// same path; the Store rejects clobbering writes otherwise. Size is the
// declared content length and may be zero when unknown at AddFile time.
type File struct {
	Version        Version
	Name           string
	MediaType      string
	Digest         string
	Size           int64
	AllowOverwrite bool
}

// Version builds a Version of p.
func (p Package) Version(name string) Version {
	return Version{Package: p, Name: name}
}

// Tag builds a Tag of p named name that points at target.
func (p Package) Tag(name, target string) Tag {
	return Tag{Package: p, Name: name, Target: target}
}

// File builds a File within v.
func (v Version) File(name string) File {
	return File{Version: v, Name: name}
}

// Tag builds a Tag named name that points at v.
func (v Version) Tag(name string) Tag {
	return Tag{Package: v.Package, Name: name, Target: v.Name}
}
