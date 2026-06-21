package filesystem_ext4

import (
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

// freeBlocksFromBitmaps recomputes the total free-block count by summing the
// zero bits across every block group's on-disk block bitmap. This is the same
// quantity e2fsck recomputes and compares against the superblock's
// s_free_blocks_count.
func freeBlocksFromBitmaps(t *testing.T, fs *ext4FS) uint64 {
	t.Helper()
	sb := fs.sb
	var total uint64
	for g := uint32(0); g < sb.numBlockGroups(); g++ {
		d, err := readBGD(fs.f, fs.partOffset, sb, g)
		if err != nil {
			t.Fatalf("readBGD %d: %v", g, err)
		}
		bmap, err := readBitmap(fs.f, fs.partOffset, sb, d.BlockBitmapBlock)
		if err != nil {
			t.Fatalf("readBitmap %d: %v", g, err)
		}
		total += uint64(countZeroBits(bmap, int(sb.BlocksPerGroup)))
	}
	return total
}

// TestSymlinkSlowFreeBlockCount guards against the slow-symlink (>60-byte,
// block-backed) path leaving the superblock's free-block count out of sync
// with the block bitmap. Before the fix the allocator wrote the superblock
// twice (once during inode allocation while the count was still one too high,
// once during block allocation with the corrected count) with no ordering
// between the two writes, so the stale value could land last and leave
// s_free_blocks_count off by one — which e2fsck reports as
// "Free blocks count wrong". This asserts the same invariant without needing
// e2fsck: the on-disk superblock must equal the bitmap recount.
func TestSymlinkSlowFreeBlockCount(t *testing.T) {
	fs, cleanup := NewTempFS(t)
	defer cleanup()

	target := "/" + strings.Repeat("abcdefghij/", 12) + "leaf" // 137 bytes > 60
	if len(target) <= 60 {
		t.Fatalf("target too short (%d) to exercise the slow path", len(target))
	}
	if err := fs.Symlink(target, "/longlink"); err != nil {
		t.Fatalf("Symlink: %v", err)
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
