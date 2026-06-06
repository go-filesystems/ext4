package filesystem_ext4

import (
	"os"
	"strings"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

// TestSetLabel_RoundTrip covers the happy path: stamp a label, re-open
// the image from scratch, confirm the on-disk superblock decodes back
// to the same value (i.e. the write went through *and* the metadata_csum
// fixup keeps the superblock valid).
func TestSetLabel_RoundTrip(t *testing.T) {
	fs, cleanup := NewTempFS(t)
	defer cleanup()

	want := "cloudimg-rootfs"
	if err := fs.SetLabel(want); err != nil {
		t.Fatalf("SetLabel(%q) failed: %v", want, err)
	}
	if got := fs.Label(); got != want {
		t.Errorf("in-memory Label() = %q, want %q", got, want)
	}

	// Re-open from disk to make sure the bytes really landed.
	path := osPathFromFS(t, fs)
	if err := fs.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened, err := Open(path, -1)
	if err != nil {
		t.Fatalf("re-Open after SetLabel: %v", err)
	}
	defer reopened.Close()
	l, ok := reopened.(filesystem.Labeller)
	if !ok {
		t.Fatal("reopened filesystem does not implement Labeller")
	}
	if got := l.Label(); got != want {
		t.Errorf("on-disk Label() after reopen = %q, want %q", got, want)
	}
}

func TestSetLabel_Empty(t *testing.T) {
	fs, cleanup := NewTempFS(t)
	defer cleanup()
	if err := fs.SetLabel("temporary"); err != nil {
		t.Fatal(err)
	}
	if err := fs.SetLabel(""); err != nil {
		t.Fatalf("SetLabel(\"\") failed: %v", err)
	}
	if got := fs.Label(); got != "" {
		t.Errorf("Label() after clear = %q, want empty", got)
	}
}

func TestSetLabel_TooLong(t *testing.T) {
	fs, cleanup := NewTempFS(t)
	defer cleanup()
	tooLong := strings.Repeat("x", MaxLabelLen+1)
	if err := fs.SetLabel(tooLong); err == nil {
		t.Fatalf("SetLabel(%d bytes) should have failed", len(tooLong))
	}
}

// osPathFromFS digs the underlying *os.File path out of a temp-formatted
// ext4 filesystem. NewTempFS wraps an osFileDevice; we use the helper
// it exposes (UnderlyingFile / Name).
func osPathFromFS(t *testing.T, fs *Ext4FS) string {
	t.Helper()
	type named interface{ Name() string }
	if n, ok := fs.f.(named); ok {
		return n.Name()
	}
	t.Fatal("ext4FS device does not expose a Name() — adjust the test helper")
	return ""
}

// Sanity check: the variable type name in test helpers (Ext4FS, exported)
// matches the assertion in NewTempFS so we don't have to drift if the
// internal name changes.
var _ *Ext4FS = (*ext4FS)(nil)
var _ = os.Open // keep "os" referenced if a future tweak removes it
