package npm

import (
	"crypto"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/yolocs/open-artifact/pkg/core"
)

func TestParsePackageName(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		raw     string
		want    PackageName
		wantErr bool
	}{
		{name: "unscoped", raw: "left-pad", want: PackageName{Original: "left-pad", Name: "left-pad"}},
		{name: "scoped", raw: "@scope/pkg", want: PackageName{Original: "@scope/pkg", Scope: "scope", Name: "pkg"}},
		{name: "dotted", raw: "a.b_c-d", want: PackageName{Original: "a.b_c-d", Name: "a.b_c-d"}},
		{name: "empty", raw: "", wantErr: true},
		{name: "uppercase", raw: "Left-Pad", wantErr: true},
		{name: "space", raw: "left pad", wantErr: true},
		{name: "tilde", raw: "left~pad", wantErr: true},
		{name: "leading dot", raw: ".left-pad", wantErr: true},
		{name: "leading underscore", raw: "_left-pad", wantErr: true},
		{name: "traversal", raw: "..", wantErr: true},
		{name: "scope missing name", raw: "@scope", wantErr: true},
		{name: "scope empty", raw: "@/pkg", wantErr: true},
		{name: "scope leading dot", raw: "@.scope/pkg", wantErr: true},
		{name: "scope name leading underscore", raw: "@scope/_pkg", wantErr: true},
		{name: "double slash", raw: "@scope/a/b", wantErr: true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParsePackageName(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParsePackageName(%q) = %+v, want error", tc.raw, got)
				}
				if !errors.Is(err, core.ErrInvalidName) {
					t.Fatalf("error = %v, want core.ErrInvalidName", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParsePackageName(%q): %v", tc.raw, err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCoreNameRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		raw      string
		wantCore string
	}{
		{raw: "left-pad", wantCore: "u/left-pad"},
		{raw: "@scope/pkg", wantCore: "s/scope/pkg"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.raw, func(t *testing.T) {
			t.Parallel()
			pn, err := ParsePackageName(tc.raw)
			if err != nil {
				t.Fatalf("ParsePackageName: %v", err)
			}
			if pn.Core() != tc.wantCore {
				t.Fatalf("Core() = %q, want %q", pn.Core(), tc.wantCore)
			}
			back, err := DecodeCorePackage(pn.Core())
			if err != nil {
				t.Fatalf("DecodeCorePackage: %v", err)
			}
			if back.Original != tc.raw {
				t.Fatalf("round trip = %q, want %q", back.Original, tc.raw)
			}
		})
	}
}

func TestValidateVersionTagFilename(t *testing.T) {
	t.Parallel()

	if err := ValidateVersion("1.2.3-beta.1+build.5"); err != nil {
		t.Fatalf("ValidateVersion(semver): %v", err)
	}
	for _, bad := range []string{"", ".hidden", "..", "1.0.0/x", "a b"} {
		if err := ValidateVersion(bad); err == nil {
			t.Fatalf("ValidateVersion(%q) = nil, want error", bad)
		}
	}
	if err := ValidateTag("latest"); err != nil {
		t.Fatalf("ValidateTag(latest): %v", err)
	}
	for _, bad := range []string{"", ".x", "a/b", ".."} {
		if err := ValidateTag(bad); err == nil {
			t.Fatalf("ValidateTag(%q) = nil, want error", bad)
		}
	}
	if err := ValidateTarballName("left-pad-1.0.0.tgz"); err != nil {
		t.Fatalf("ValidateTarballName: %v", err)
	}
	for _, bad := range []string{"", "left-pad-1.0.0.zip", "../escape.tgz", ".hidden.tgz", "a/b.tgz"} {
		if err := ValidateTarballName(bad); err == nil {
			t.Fatalf("ValidateTarballName(%q) = nil, want error", bad)
		}
	}
}

func TestExpectedDigests(t *testing.T) {
	t.Parallel()

	tarball := []byte("the tarball bytes")
	sha1sum := sha1.Sum(tarball)
	sha512sum := sha512.Sum512(tarball)
	shasum := hex.EncodeToString(sha1sum[:])
	integrity := "sha512-" + base64.StdEncoding.EncodeToString(sha512sum[:])

	// Both declarations present -> two checks with the raw digest bytes.
	got, err := expectedDigests(map[string]any{"shasum": shasum, "integrity": integrity})
	if err != nil {
		t.Fatalf("expectedDigests: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d digests, want 2", len(got))
	}
	want := []core.ExpectedDigest{
		{Hash: crypto.SHA1, Sum: sha1sum[:]},
		{Hash: crypto.SHA512, Sum: sha512sum[:]},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("digests (-want +got):\n%s", diff)
	}

	// Absent dist / values -> no checks.
	if got, err := expectedDigests(nil); err != nil || got != nil {
		t.Fatalf("expectedDigests(nil) = %v, %v; want nil, nil", got, err)
	}

	// A non-sha512 integrity is ignored (npm always sends sha512).
	if got, err := expectedDigests(map[string]any{"integrity": "sha1-AAAA"}); err != nil || got != nil {
		t.Fatalf("expectedDigests(sha1 integrity) = %v, %v; want nil, nil", got, err)
	}

	// Malformed declarations are rejected.
	for _, dist := range []map[string]any{
		{"shasum": "nothex!!"},
		{"integrity": "sha512-not base64"},
	} {
		if _, err := expectedDigests(dist); !errors.Is(err, errBadDigest) {
			t.Fatalf("expectedDigests(%v) err = %v, want errBadDigest", dist, err)
		}
	}
}
