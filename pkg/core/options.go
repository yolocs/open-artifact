package core

import "crypto"

// ExpectedDigest is a content hash the Store verifies while streaming a file's
// body. The Store computes Hash over the bytes and checks the result equals
// Sum; a mismatch aborts the write — nothing is committed — and AddFile returns
// ErrDigestMismatch. Verifying during the streamed write (rather than in a
// separate pass by the caller) means a corrupt upload never leaves a partial,
// servable blob, and the caller need not buffer the body to hash it. The
// algorithm's implementation must be linked into the binary (e.g. an import of
// crypto/sha1); an unavailable hash yields ErrUnsupported.
type ExpectedDigest struct {
	Hash crypto.Hash
	Sum  []byte
}

// CreateConfig holds the creation-time options resolved from a set of
// CreateOption values. Store implementations build one with NewCreateConfig
// and read its fields rather than interpreting CreateOption directly.
type CreateConfig struct {
	// Annotations are caller-owned key/values stored verbatim in the new
	// record's Meta.Annotations.
	Annotations map[string]any

	// AllowOverwrite permits a create operation to clobber an existing
	// record. When false (the default), writing over an existing record
	// returns ErrAlreadyExists.
	AllowOverwrite bool

	// Expected lists content hashes to verify against the streamed body before
	// committing. It applies to AddFile (and AddCache); a mismatch aborts the
	// write with ErrDigestMismatch.
	Expected []ExpectedDigest
}

// CreateOption customizes a create operation (AddPackage, AddVersion,
// AddFile).
type CreateOption func(*CreateConfig)

// WithAnnotations attaches caller-owned annotations to the record being
// created. Repeated options merge; later keys win.
func WithAnnotations(annotations map[string]any) CreateOption {
	return func(c *CreateConfig) {
		if len(annotations) == 0 {
			return
		}
		if c.Annotations == nil {
			c.Annotations = make(map[string]any, len(annotations))
		}
		for k, v := range annotations {
			c.Annotations[k] = v
		}
	}
}

// WithAllowOverwrite controls whether a create operation may clobber an
// existing record. The default (no option, or false) returns ErrAlreadyExists
// rather than overwriting.
func WithAllowOverwrite(allow bool) CreateOption {
	return func(c *CreateConfig) {
		c.AllowOverwrite = allow
	}
}

// WithExpectedDigests asks the Store to verify the streamed body against the
// given content hashes before committing, aborting with ErrDigestMismatch on
// any mismatch. The Store's own canonical SHA-256 digest is always computed
// regardless. Repeated options accumulate.
func WithExpectedDigests(expected ...ExpectedDigest) CreateOption {
	return func(c *CreateConfig) {
		c.Expected = append(c.Expected, expected...)
	}
}

// NewCreateConfig resolves opts into a CreateConfig.
func NewCreateConfig(opts ...CreateOption) CreateConfig {
	var c CreateConfig
	for _, opt := range opts {
		opt(&c)
	}
	return c
}
