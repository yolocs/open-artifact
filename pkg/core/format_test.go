package core_test

import (
	"testing"

	"github.com/yolocs/open-artifact/pkg/core"
)

func TestFormatIsKnown(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		format core.Format
		want   bool
	}{
		{name: "pypi", format: core.FormatPyPI, want: true},
		{name: "npm", format: core.FormatNPM, want: true},
		{name: "maven", format: core.FormatMaven, want: true},
		{name: "debian", format: core.FormatDebian, want: true},
		{name: "generic", format: core.FormatGeneric, want: true},
		{name: "empty", format: core.Format(""), want: false},
		{name: "unknown", format: core.Format("cargo"), want: false},
		{name: "wrong case", format: core.Format("PyPI"), want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := tc.format.IsKnown(); got != tc.want {
				t.Errorf("Format(%q).IsKnown() = %t, want %t", tc.format, got, tc.want)
			}
		})
	}
}

func TestFormatStringRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want core.Format
		ok   bool
	}{
		{name: "pypi", in: "pypi", want: core.FormatPyPI, ok: true},
		{name: "npm", in: "npm", want: core.FormatNPM, ok: true},
		{name: "maven", in: "maven", want: core.FormatMaven, ok: true},
		{name: "debian", in: "debian", want: core.FormatDebian, ok: true},
		{name: "generic", in: "generic", want: core.FormatGeneric, ok: true},
		{name: "unknown", in: "cargo", want: core.Format("cargo"), ok: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, ok := core.ParseFormat(tc.in)
			if got != tc.want || ok != tc.ok {
				t.Errorf("ParseFormat(%q) = (%q, %t), want (%q, %t)", tc.in, got, ok, tc.want, tc.ok)
			}
			if rt := got.String(); rt != tc.in {
				t.Errorf("Format(%q).String() = %q, want %q", got, rt, tc.in)
			}
		})
	}
}
