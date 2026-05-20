package core

import "errors"

// Sentinel errors the Store returns. Surfaces match them with errors.Is and
// map them to the format's HTTP error shape via a shared surface helper.
var (
	// ErrNotFound is returned when a requested record does not exist.
	ErrNotFound = errors.New("open-artifact: not found")
	// ErrAlreadyExists is returned when a write would clobber an existing
	// record and the caller did not opt in to overwriting.
	ErrAlreadyExists = errors.New("open-artifact: already exists")
	// ErrDigestMismatch is returned when uploaded content does not match the
	// caller-declared digest.
	ErrDigestMismatch = errors.New("open-artifact: digest mismatch")
	// ErrUnsupported is returned for operations a backend cannot perform.
	ErrUnsupported = errors.New("open-artifact: unsupported")
)
