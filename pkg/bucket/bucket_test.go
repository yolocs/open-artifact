package bucket

import (
	"net/url"
	"testing"
)

func TestOpen(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{name: "memblob", url: "mem://"},
		{name: "unregistered scheme", url: "bogus://nowhere", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b, cleanup, err := Open(t.Context(), tc.url)
			if tc.wantErr {
				if err == nil {
					cleanup()
					t.Fatalf("Open(%q) = nil error, want error", tc.url)
				}
				if cleanup != nil {
					t.Error("Open returned non-nil cleanup alongside an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Open(%q): %v", tc.url, err)
			}
			defer cleanup()
			if b == nil {
				t.Fatal("Open returned nil bucket")
			}
			if err := b.WriteAll(t.Context(), "probe", []byte("ok"), nil); err != nil {
				t.Fatalf("WriteAll on opened bucket: %v", err)
			}
		})
	}
}

func TestOpenFileblob(t *testing.T) {
	t.Parallel()

	// file:// requires an absolute path; build it from the test temp dir.
	u := url.URL{Scheme: "file", Path: t.TempDir()}
	b, cleanup, err := Open(t.Context(), u.String())
	if err != nil {
		t.Fatalf("Open(%q): %v", u.String(), err)
	}
	defer cleanup()

	if err := b.WriteAll(t.Context(), "probe", []byte("ok"), nil); err != nil {
		t.Fatalf("WriteAll on fileblob bucket: %v", err)
	}
	got, err := b.ReadAll(t.Context(), "probe")
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "ok" {
		t.Errorf("ReadAll = %q, want %q", got, "ok")
	}
}
