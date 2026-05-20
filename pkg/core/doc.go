// Package core is the internal substrate of open-artifact: the data nouns
// (Package, Version, Tag, File), the Format enum, the scope-blind Store
// interface, the Meta envelope, and the package's sentinel errors.
//
// core is what artifact records *are*. Nothing here knows about HTTP
// protocols, upstream registries, or any concrete storage backend. The
// blob-backed Store lives in the core/blobstore subpackage; protocol
// surfaces live under pkg/surface and depend on core only.
package core
