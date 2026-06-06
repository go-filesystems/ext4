package filesystem_ext4_test

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	ext4 "github.com/go-filesystems/ext4"
)

// TestWriteFile_ImmediateReadAtomic performs concurrent writes and immediate
// reads to verify that a WriteFile that uses journaling makes the file
// visible atomically (no ReadFile ENOENT after WriteFile returns).
func TestWriteFile_ImmediateReadAtomic(t *testing.T) {
	t.Helper()

	// Install a CommitHook to log transactions for diagnostic correlation.
	// Preserve any existing hook and restore on test exit.
	prevHook := ext4.CommitHook
	ext4.CommitHook = func(seq uint64, entries int) {
		t.Logf("COMMITHOOK seq=%d entries=%d", seq, entries)
	}
	defer func() { ext4.CommitHook = prevHook }()

	path := filepath.Join(t.TempDir(), "disk.img")
	// Use a larger image to avoid allocation exhaustion during concurrent writes.
	// Increase to 256 MiB to provide more block groups and inodes for stress runs.
	if _, err := ext4.Format(path, 4096*65536, ext4.FormatConfig{}); err != nil {
		t.Fatalf("Format: %v", err)
	}
	// Ensure a sidecar journal file exists so Open enables journaling.
	if f, err := os.Create(path + ".journal"); err != nil {
		t.Fatalf("create journal sidecar: %v", err)
	} else {
		f.Close()
	}

	fs, err := ext4.Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	const workers = 4
	const perWorker = 20

	var wg sync.WaitGroup
	var errs atomic.Int64

	for w := 0; w < workers; w++ {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				name := fmt.Sprintf("/atomic-w%d-i%03d.txt", w, i)
				if err := fs.WriteFile(name, []byte("payload"), 0o644); err != nil {
					t.Logf("WriteFile error name=%s: %v", name, err)
					errs.Add(1)
					return
				}
				if _, err := fs.ReadFile(name); err != nil {
					// Treat any error as a failure for this atomicity check.
					t.Logf("ReadFile after WriteFile failed name=%s: %v", name, err)
					errs.Add(1)
					return
				}
			}
		}()
	}
	wg.Wait()

	if c := errs.Load(); c != 0 {
		t.Fatalf("observed %d read-after-write errors", c)
	}
}
