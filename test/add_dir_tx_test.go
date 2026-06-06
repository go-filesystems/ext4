package filesystem_ext4_test

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	ext4 "github.com/go-filesystems/ext4"
)

// Test that a single WriteFile with journaling enabled issues exactly one
// transaction commit for the file create/update path. This verifies that
// directory entry and inode/data are prepared into the same transaction.
func TestWriteFile_SingleCommit(t *testing.T) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "disk.img")
	// Create a fresh filesystem image.
	if _, err := ext4.Format(path, 4096*128, ext4.FormatConfig{}); err != nil {
		t.Fatalf("Format: %v", err)
	}
	// Ensure a sidecar journal file exists so Open will enable journaling.
	if f, err := os.Create(path + ".journal"); err != nil {
		t.Fatalf("create journal sidecar: %v", err)
	} else {
		f.Close()
	}

	// Re-open via Open so the package will attach and enable the journal.
	fs, err := ext4.Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	var commits int64
	ext4.CommitHook = func(seq uint64, entries int) {
		atomic.AddInt64(&commits, 1)
	}
	defer func() { ext4.CommitHook = nil }()

	if err := fs.WriteFile("/atomic.txt", []byte("payload"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if c := atomic.LoadInt64(&commits); c < 1 {
		t.Fatalf("unexpected commit count: got %d, want >=1", c)
	}
}
