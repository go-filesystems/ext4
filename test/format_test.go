package filesystem_ext4_test

import (
	"os"
	"path/filepath"
	"testing"

	ext4 "github.com/go-filesystems/ext4"
)

// ── Format ───────────────────────────────────────────────────────────────────

func TestFormat_TooSmall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tiny.img")
	if _, err := ext4.Format(path, 1024, ext4.FormatConfig{}); err == nil {
		t.Error("expected error for image smaller than minimum size")
	}
}

func TestFormat_NotMultipleOfBlockSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.img")
	if _, err := ext4.Format(path, 4097, ext4.FormatConfig{}); err == nil {
		t.Error("expected error for size not a multiple of block size")
	}
}

func TestFormat_CreatesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new.img")
	fs, err := ext4.Format(path, 20*1024*1024, ext4.FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	fs.Close()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("image file not created: %v", err)
	}
	// Cross-validate the freshly-formatted image with e2fsck.
	runE2fsck(t, path)
}

func TestFormat_FileSizePreserved(t *testing.T) {
	const size = 20 * 1024 * 1024
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := ext4.Format(path, size, ext4.FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	fs.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != size {
		t.Errorf("file size = %d, want %d", info.Size(), size)
	}
}

func TestFormat_TruncatesExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing.img")
	// Write something to the file first.
	if err := os.WriteFile(path, make([]byte, 1024*1024), 0o600); err != nil {
		t.Fatal(err)
	}
	const newSize = 20 * 1024 * 1024
	fs, err := ext4.Format(path, newSize, ext4.FormatConfig{})
	if err != nil {
		t.Fatalf("Format on existing file: %v", err)
	}
	fs.Close()
}

func TestFormat_ListDirRoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := ext4.Format(path, 20*1024*1024, ext4.FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()

	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir /: %v", err)
	}
	names := make(map[string]bool)
	for _, e := range entries {
		names[e.Name()] = true
	}
	// ListDir omits the "." and ".." self/parent links (matching the other
	// drivers and os.ReadDir); they must not appear in a listing.
	if names["."] {
		t.Error("root dir listing should not include '.' entry")
	}
	if names[".."] {
		t.Error("root dir listing should not include '..' entry")
	}
}

func TestFormat_StatRoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := ext4.Format(path, 20*1024*1024, ext4.FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()

	st, err := fs.Stat("/")
	if err != nil {
		t.Fatalf("Stat /: %v", err)
	}
	if st.Inode() != uint64(ext4.RootIno) {
		t.Errorf("root inode = %d, want %d", st.Inode(), uint64(ext4.RootIno))
	}
	if st.Mode()&0xF000 != 0x4000 {
		t.Errorf("root mode 0x%04X is not a directory (0x4000…)", st.Mode())
	}
}

func TestFormat_WriteReadRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := ext4.Format(path, 20*1024*1024, ext4.FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}

	const content = "hello from Format\n"
	if err := fs.WriteFile("/hello.txt", []byte(content), 0o644); err != nil {
		fs.Close()
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := fs.ReadFile("/hello.txt")
	if err != nil {
		fs.Close()
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != content {
		t.Errorf("ReadFile: got %q, want %q", got, content)
	}
	// Cross-validate the on-disk image with upstream e2fsck. Skipped when
	// e2fsck is not installed.
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	runE2fsck(t, path)
}

func TestFormat_WriteMultipleFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := ext4.Format(path, 20*1024*1024, ext4.FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}

	files := map[string]string{
		"/a.txt": "content of a",
		"/b.txt": "content of b",
		"/c.txt": "content of c",
	}
	for name, data := range files {
		if err := fs.WriteFile(name, []byte(data), 0o644); err != nil {
			fs.Close()
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}
	for name, want := range files {
		got, err := fs.ReadFile(name)
		if err != nil {
			fs.Close()
			t.Fatalf("ReadFile %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("%s: got %q, want %q", name, got, want)
		}
	}
	// Cross-validate with e2fsck (skipped when binary unavailable).
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	runE2fsck(t, path)
}

func TestFormat_ReadNonExistentFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := ext4.Format(path, 20*1024*1024, ext4.FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()

	if _, err := fs.ReadFile("/does_not_exist.txt"); err == nil {
		t.Error("expected error reading non-existent file")
	}
}

func TestFormat_CustomLabel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := ext4.Format(path, 20*1024*1024, ext4.FormatConfig{Label: "my-volume"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	fs.Close()
	// A re-opened filesystem should still be valid.
	fs2, err := ext4.Open(path, -1)
	if err != nil {
		t.Fatalf("Open after Format with label: %v", err)
	}
	defer fs2.Close()
	if _, err := fs2.ListDir("/"); err != nil {
		t.Fatalf("ListDir after re-open: %v", err)
	}
}

func TestFormat_ReOpenAndWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	// Format and write a file.
	{
		fs, err := ext4.Format(path, 20*1024*1024, ext4.FormatConfig{})
		if err != nil {
			t.Fatalf("Format: %v", err)
		}
		if err := fs.WriteFile("/data.bin", []byte("original"), 0o600); err != nil {
			fs.Close()
			t.Fatalf("WriteFile: %v", err)
		}
		fs.Close()
	}
	// Re-open and read the file back.
	fs, err := ext4.Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, err := fs.ReadFile("/data.bin")
	if err != nil {
		fs.Close()
		t.Fatalf("ReadFile after re-open: %v", err)
	}
	if string(got) != "original" {
		t.Errorf("got %q, want \"original\"", got)
	}
	// Cross-validate the image after Format→close→Open→read→close with e2fsck.
	if err := fs.Close(); err != nil {
		t.Fatalf("Close after re-open: %v", err)
	}
	runE2fsck(t, path)
}

func TestFormat_MultiGroup(t *testing.T) {
	// 256 MiB > 128 MiB (one block group), forces nGroups > 1.
	path := filepath.Join(t.TempDir(), "big.img")
	const size = 256 * 1024 * 1024
	fs, err := ext4.Format(path, size, ext4.FormatConfig{})
	if err != nil {
		t.Fatalf("Format 256 MiB: %v", err)
	}

	const content = "multi-group test"
	if err := fs.WriteFile("/multi.txt", []byte(content), 0o644); err != nil {
		fs.Close()
		t.Fatalf("WriteFile in multi-group image: %v", err)
	}
	got, err := fs.ReadFile("/multi.txt")
	if err != nil {
		fs.Close()
		t.Fatalf("ReadFile in multi-group image: %v", err)
	}
	if string(got) != content {
		t.Errorf("got %q, want %q", got, content)
	}
	// Cross-validate with e2fsck — catches BGD/bitmap issues across groups.
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	runE2fsck(t, path)
}
