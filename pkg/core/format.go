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
)

// IsKnown reports whether f is a Format this build understands.
func (f Format) IsKnown() bool {
	switch f {
	case FormatPyPI, FormatNPM, FormatMaven, FormatDebian:
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
