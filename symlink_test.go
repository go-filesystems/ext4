package filesystem_ext4

import (
	"path/filepath"
	"strings"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

// TestSymlinkFastCreate creates a short-target (fast) symlink and reads it back.
func TestSymlinkFastCreate(t *testing.T) {
	fs, cleanup := NewTempFS(t)
	defer cleanup()

	var _ filesystem.Symlinker = fs // capability is exposed

	if err := fs.MkDir("/d", 0o755); err != nil {
		t.Fatalf("MkDir: %v", err)
	}
	if err := fs.WriteFile("/d/target.txt", []byte("hi"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := fs.Symlink("target.txt", "/d/link"); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	got, err := fs.ReadLink("/d/link")
	if err != nil {
		t.Fatalf("ReadLink: %v", err)
	}
	if got != "target.txt" {
		t.Fatalf("ReadLink = %q, want %q", got, "target.txt")
	}
	// The directory entry must be typed as a symlink (Stat follows the link,
	// so it reports the target's mode; the dirent file-type is the lstat-ish
	// view).
	entries, err := fs.ListDir("/d")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	var ft uint8
	found := false
	for _, e := range entries {
		if e.Name() == "link" {
			ft, found = e.FileType(), true
		}
	}
	if !found {
		t.Fatal("link entry not found in /d")
	}
	if ft != FtSymlink {
		t.Fatalf("link dirent file_type = %d, want FtSymlink (%d)", ft, FtSymlink)
	}
	// Following the link resolves to the target file's contents.
	if data, err := fs.ReadFile("/d/link"); err != nil || string(data) != "hi" {
		t.Fatalf("ReadFile via symlink = %q, %v; want \"hi\"", data, err)
	}
	// Creating over an existing name must fail.
	if err := fs.Symlink("whatever", "/d/link"); err == nil {
		t.Fatal("Symlink over existing name: expected error")
	}
}

// TestSymlinkSlowCreate creates a long-target (slow, block-backed) symlink and
// reads it back, exercising the >60-byte storage path.
func TestSymlinkSlowCreate(t *testing.T) {
	fs, cleanup := NewTempFS(t)
	defer cleanup()

	target := "/" + strings.Repeat("abcdefghij/", 12) + "leaf" // 137 bytes > 60
	if len(target) <= 60 {
		t.Fatalf("test target too short (%d) to exercise the slow path", len(target))
	}
	if err := fs.Symlink(target, "/longlink"); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	got, err := fs.ReadLink("/longlink")
	if err != nil {
		t.Fatalf("ReadLink: %v", err)
	}
	if got != target {
		t.Fatalf("ReadLink = %q, want %q", got, target)
	}
}

// TestSymlinkReopenPersist confirms a created symlink survives a close/re-open
// cycle (the on-disk inode + dirent must be self-sufficient).
func TestSymlinkReopenPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sl.img")
	fsi, err := Format(path, 4096*4096, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	fs := fsi.(*ext4FS)
	const target = "/etc/hosts"
	if err := fs.Symlink(target, "/hostlink"); err != nil {
		fs.Close()
		t.Fatalf("Symlink: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	fsi2, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open(reopen): %v", err)
	}
	defer fsi2.Close()
	got, err := fsi2.ReadLink("/hostlink")
	if err != nil {
		t.Fatalf("ReadLink after reopen: %v", err)
	}
	if got != target {
		t.Fatalf("post-reopen ReadLink = %q, want %q", got, target)
	}
}
