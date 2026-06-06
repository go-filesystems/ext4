package filesystem_ext4_test

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	ext4 "github.com/go-filesystems/ext4"
)

// TestConcurrentDirAdds performs many concurrent creations in the same
// directory to exercise directory-entry allocation and the grouped
// directory-block+inode transaction path.
func TestConcurrentDirAdds(t *testing.T) {
	// Reuse the stress image resolution helpers; skip if none available.
	spec := allStressImages[0]
	src := resolveStressImage(t, spec)
	if src == "" {
		t.Skipf("no image found in cache for %s", spec.distro)
	}
	raw := toStressRaw(t, src, spec.format)
	partIdx := findExt4Partition(t, raw)

	tmp := copyStressImage(t, raw)
	fs, err := ext4.Open(tmp, partIdx)
	if err != nil {
		t.Fatalf("ext4.Open: %v", err)
	}
	defer fs.Close()

	const workers = 16
	const perWorker = 200

	var wg sync.WaitGroup
	var errCount atomic.Int64

	for w := 0; w < workers; w++ {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				path := fmt.Sprintf("/etc/conc-add-w%d-i%03d.txt", w, i)
				if err := fs.WriteFile(path, []byte("hello"), 0o644); err != nil {
					errCount.Add(1)
					return
				}
			}
		}()
	}
	wg.Wait()

	if c := errCount.Load(); c != 0 {
		t.Fatalf("concurrent dir adds: %d errors", c)
	}
}
