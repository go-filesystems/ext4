package filesystem_ext4_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	ext4 "github.com/go-filesystems/ext4"
	filesystem "github.com/go-filesystems/interface"
)

func newAtomicIndirectFS(t *testing.T) filesystem.Filesystem {
	t.Helper()
	path := filepath.Join(t.TempDir(), "disk.img")
	if _, err := ext4.Format(path, 4096*131072, ext4.FormatConfig{}); err != nil {
		t.Fatalf("Format: %v", err)
	}
	if f, err := os.Create(path + ".journal"); err != nil {
		t.Fatalf("create journal sidecar: %v", err)
	} else {
		f.Close()
	}
	fs, err := ext4.Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	return fs

}

// TestWriteFile_ImmediateReadAtomic_IndirectDir forces a freshly formatted
// directory past four data blocks so the parent inode switches to an indexed
// extent tree. Each write must remain immediately visible.
func TestWriteFile_ImmediateReadAtomic_IndirectDir(t *testing.T) {
	fs := newAtomicIndirectFS(t)

	const writes = 700
	for i := 0; i < writes; i++ {
		name := fmt.Sprintf("/indirect-i%04d.txt", i)
		want := []byte(fmt.Sprintf("payload-%04d", i))
		if err := fs.WriteFile(name, want, 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", name, err)
		}
		got, err := fs.ReadFile(name)
		if err != nil {
			t.Fatalf("ReadFile(%s) after write: %v", name, err)
		}
		if string(got) != string(want) {
			t.Fatalf("ReadFile(%s): got %q, want %q", name, got, want)
		}
	}
}
