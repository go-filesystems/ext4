package filesystem_ext4

import (
	"sync/atomic"
	"testing"
)

// TestMkDirFreeBlockCount guards against the non-journal makeDir path leaving
// the superblock's free-block count out of sync with the block bitmap. A
// directory creation allocates the inode first and the directory data block
// second. Before the fix both allocInode and allocBlocks independently
// snapshotted and wrote the entire superblock to the fixed offset 1024:
// allocInode wrote it while s_free_blocks_count was still one too high, and
// allocBlocks wrote it again after decrementing the count, with no ordering
// between the two writes — so the stale (higher) value could land last and
// leave s_free_blocks_count off by one, which e2fsck reports as "Free blocks
// count wrong". This asserts the same invariant without needing e2fsck: the
// on-disk superblock must equal the bitmap recount.
func TestMkDirFreeBlockCount(t *testing.T) {
	fs, cleanup := NewTempFS(t)
	defer cleanup()

	if err := fs.MkDir("/newdir", 0o755); err != nil {
		t.Fatalf("MkDir: %v", err)
	}

	wantFree := freeBlocksFromBitmaps(t, fs)
	// Re-read the superblock from disk — this is what e2fsck inspects, and is
	// distinct from the in-memory counter which was already correct.
	disk, err := readSuperblock(fs.f, fs.partOffset)
	if err != nil {
		t.Fatalf("readSuperblock: %v", err)
	}
	if disk.FreeBlocksCount != wantFree {
		t.Fatalf("on-disk superblock free-block count = %d, bitmap recount = %d (off by %d)",
			disk.FreeBlocksCount, wantFree, int64(disk.FreeBlocksCount)-int64(wantFree))
	}
	// The in-memory counter must agree too.
	if mem := atomic.LoadUint64(&fs.sb.FreeBlocksCount); mem != wantFree {
		t.Fatalf("in-memory free-block count = %d, bitmap recount = %d", mem, wantFree)
	}
}
