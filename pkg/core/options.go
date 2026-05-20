package core

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

// NewCreateConfig resolves opts into a CreateConfig.
func NewCreateConfig(opts ...CreateOption) CreateConfig {
	var c CreateConfig
	for _, opt := range opts {
		opt(&c)
	}
	return c
}
