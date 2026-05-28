// Package generic implements open-artifact's native REST artifact registry
// surface. Unlike the package-manager formats (pypi, npm, maven, debian) there
// is no external wire protocol to conform to: the generic format defines its own
// small REST API mapped almost directly onto the core Package/Version/File
// nouns, so the only format-specific code is the routing and the JSON metadata
// representation.
//
// It is hosted-only in v1. A proxy-mode generic namespace has no standard
// upstream layout to pull through, so proxy requests return 501; proxy support
// is a separate design. Deletion of packages/versions/files is deferred — the
// pure core has no Delete verbs yet — so DELETE routes are unregistered and
// answer 405 (with Allow) until that design pass lands.
//
// Routes live under /{namespace}/generic:
//
//	GET  /packages                                           list package names
//	GET  /packages/{package}                                 package metadata + version names
//	PUT  /packages/{package}                                 create/update a package (optional annotations body)
//	GET  /packages/{package}/versions                        list version names
//	GET  /packages/{package}/versions/{version}              version metadata + file names
//	PUT  /packages/{package}/versions/{version}              create/update a version (optional annotations body)
//	GET  /packages/{package}/versions/{version}/files        list files (name, size, digest, content_type)
//	PUT  /packages/{package}/versions/{version}/files/{file} upload a file (streamed)
//	GET  /packages/{package}/versions/{version}/files/{file} download a file (redirect-or-stream; HEAD supported)
//
// Packages and versions are idempotent containers: PUT upserts the metadata
// envelope (annotations are replaced wholesale) and returns 201 on create, 200
// on update. Files are immutable content: PUT is create-once and returns 409 on
// re-upload unless --generic-allow-overwrite is set. Uploading a file
// auto-creates its parent package and version.
//
// Collection endpoints (the list routes) return a possibly-empty list with 200
// even when the parent is absent; single-resource GETs (a package or a version)
// return 404 when absent.
//
// Upload bodies are streamed straight into the Store (rolling SHA-256 during the
// write, no buffering) and capped by --generic-max-upload-bytes. Optional
// integrity headers X-Checksum-Sha256/-Sha1/-Sha512 (lowercase hex) are verified
// during the streamed write; a mismatch aborts before commit (422). The upload
// Content-Type is stored and served back on download.
//
// Names (package, version, file) are single path segments — percent-encode any
// reserved characters. The surface forwards names to the Store unchanged;
// blobstore owns path-safe encoding and rejects empty or leading-dot names.
package generic
