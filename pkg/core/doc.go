// Package core is the internal substrate of open-artifact: the data nouns
// (Package, Version, Tag, File), the Format enum, the scope-blind Store
// interface, the Meta envelope, and the package's sentinel errors.
//
// core is what artifact records *are*. Nothing here knows about HTTP
// protocols, upstream registries, or any concrete storage backend. The
// blob-backed Store lives in the core/blobstore subpackage; protocol
// surfaces live under pkg/surface and depend on core only.
//
// The nouns are chainable handles, not value structs. A handle is obtained
// without I/O; existence and contents are observed only when a
// context-taking method is called. Handles compose downward from a Store:
//
//	file := store.Package("requests").Version("2.31.0").
//		File("requests-2.31.0-py3-none-any.whl")
//	rc, err := file.Read(ctx)
package core
