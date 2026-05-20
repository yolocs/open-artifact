// Package blobstore implements core.Store on top of a gocloud.dev/blob
// *blob.Bucket. The bucket is the storage driver: memblob and fileblob for
// tests and local use, s3blob/gcsblob/azureblob in production.
//
// Each Store instance is per-scope (a path prefix configured at
// construction, e.g. "pypi/global") and owns the on-bucket path scheme,
// the .meta/.tags dot-file conventions, streaming uploads with rolling
// SHA256, and the facade-transparent SignedURL / Stat caches.
package blobstore
