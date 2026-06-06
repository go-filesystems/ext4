package filesystem_ext4_test

import (
	"os"
	"sync"
	"sync/atomic"
	"testing"

	ext4 "github.com/go-filesystems/ext4"
)

// TestJournalCommitConcurrency exercises concurrent Transaction.Commit()
// against a real sidecar journal to ensure the recent Commit() refactor
// does not introduce data races under -race.
func TestJournalCommitConcurrency(t *testing.T) {
	// Disable fsync to keep the test fast and deterministic in CI.
	t.Setenv("EXT4_DISABLE_FSYNC", "1")

	tmp, err := os.CreateTemp("", "ext4journal")
	if err != nil {
		t.Fatal(err)
	}
	path := tmp.Name()
	tmp.Close()
	defer os.Remove(path)

	fsIfc, err := ext4.Format(path, int64(4096*64), ext4.FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	fs, ok := fsIfc.(*ext4.Ext4FS)
	if !ok {
		t.Fatalf("Format returned unexpected FS type")
	}
	sb := ext4.CloneSuperblockFromFS(fs)

	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open file: %v", err)
	}
	defer f.Close()

	j, err := ext4.OpenJournal(f, 0, int(sb.BlockSize), sb)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	if j == nil {
		t.Fatalf("expected sidecar journal to be enabled for file")
	}

	var wg sync.WaitGroup
	var cnt uint64
	goroutines := 8
	iters := 500

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				tx, err := j.StartTx()
				if err != nil {
					t.Errorf("StartTx: %v", err)
					return
				}
				blk := atomic.AddUint64(&cnt, 1) % sb.BlocksCount
				data := make([]byte, sb.BlockSize)
				copy(data, []byte("x"))
				startAbs := int64(blk) * int64(sb.BlockSize)
				if err := ext4.AddRangeToTx(tx, f, 0, sb, startAbs, data); err != nil {
					t.Errorf("AddRangeToTx: %v", err)
					return
				}
				if err := tx.Commit(); err != nil {
					t.Errorf("Commit: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}
