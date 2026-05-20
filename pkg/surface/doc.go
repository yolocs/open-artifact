// Package surface holds the external protocol surfaces of open-artifact:
// the Handler interface and shared HTTP/error helpers. Each format gets a
// subpackage (pypi, npm, ...) pairing an inbound protocol handler with its
// outbound upstream client.
//
// surface is how clients talk to and about artifact records. Surfaces
// depend on pkg/core only; they never touch the storage backend directly.
package surface
