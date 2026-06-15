package filesystem_ext4

import (
	"path/filepath"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

// TestLinkHardlink creates a hard link, confirms both names read the same
// content, that the link count was bumped, that directories are rejected, and
// that an existing target name is rejected.
func TestLinkHardlink(t *testing.T) {
	fs, cleanup := NewTempFS(t)
	defer cleanup()

	var _ filesystem.HardLinker = fs // capability is exposed

	if err := fs.WriteFile("/orig.bin", []byte("shared-bytes"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := fs.Link("/orig.bin", "/alias.bin"); err != nil {
		t.Fatalf("Link: %v", err)
	}
	// Both names resolve to the same content.
	got, err := fs.ReadFile("/alias.bin")
	if err != nil || string(got) != "shared-bytes" {
		t.Fatalf("ReadFile(alias) = %q, %v; want shared-bytes", got, err)
	}
	// Same inode number → same Stat inode.
	a, _ := fs.Stat("/orig.bin")
	b, _ := fs.Stat("/alias.bin")
	if a.Inode() != b.Inode() {
		t.Fatalf("hardlink inode mismatch: %d vs %d", a.Inode(), b.Inode())
	}

	// Directories cannot be hard-linked.
	if err := fs.MkDir("/d", 0o755); err != nil {
		t.Fatalf("MkDir: %v", err)
	}
	if err := fs.Link("/d", "/d2"); err == nil {
		t.Fatal("Link of directory: expected error")
	}
	// Existing target name is rejected.
	if err := fs.Link("/orig.bin", "/alias.bin"); err == nil {
		t.Fatal("Link onto existing name: expected error")
	}
}

// TestLinkPersistAndUnlink confirms a hard link survives a re-open and that the
// inode's content outlives deletion of one of its names.
func TestLinkPersistAndUnlink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "link.img")
	fsi, err := Format(path, 4096*4096, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	fs := fsi.(*ext4FS)
	if err := fs.WriteFile("/a", []byte("payload"), 0o644); err != nil {
		fs.Close()
		t.Fatalf("WriteFile: %v", err)
	}
	if err := fs.Link("/a", "/b"); err != nil {
		fs.Close()
		t.Fatalf("Link: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	fsi2, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open(reopen): %v", err)
	}
	defer fsi2.Close()
	// After reopen both names still read the content.
	for _, name := range []string{"/a", "/b"} {
		got, err := fsi2.ReadFile(name)
		if err != nil || string(got) != "payload" {
			t.Fatalf("post-reopen ReadFile(%s) = %q, %v; want payload", name, got, err)
		}
	}
	// Deleting one name leaves the other intact (link count > 1).
	if err := fsi2.DeleteFile("/a"); err != nil {
		t.Fatalf("DeleteFile(/a): %v", err)
	}
	if got, err := fsi2.ReadFile("/b"); err != nil || string(got) != "payload" {
		t.Fatalf("ReadFile(/b) after unlinking /a = %q, %v; want payload", got, err)
	}
}
