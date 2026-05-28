package core

// Format identifies a package-manager protocol that a surface speaks. It is
// a string under the hood so it round-trips losslessly through config, env
// vars, and on-bucket scope prefixes.
type Format string

const (
	// FormatPyPI is the Python Package Index protocol (PEP 503/691).
	FormatPyPI Format = "pypi"
	// FormatNPM is the npm registry protocol.
	FormatNPM Format = "npm"
	// FormatMaven is the Maven 2 repository layout.
	FormatMaven Format = "maven"
	// FormatDebian is the Debian/APT repository layout (proxy-only).
	FormatDebian Format = "debian"
	// FormatGeneric is open-artifact's native REST artifact API (hosted-only in
	// v1): a thin mapping onto the core Package/Version/File nouns with no
	// external package-manager protocol.
	FormatGeneric Format = "generic"
)

// IsKnown reports whether f is a Format this build understands.
func (f Format) IsKnown() bool {
	switch f {
	case FormatPyPI, FormatNPM, FormatMaven, FormatDebian, FormatGeneric:
		return true
	default:
		return false
	}
}

// String returns the wire representation of f.
func (f Format) String() string {
	return string(f)
}

// ParseFormat converts s to a Format. The second return value reports
// whether the result is a known Format.
func ParseFormat(s string) (Format, bool) {
	f := Format(s)
	return f, f.IsKnown()
}
