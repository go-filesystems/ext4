package filesystem_ext4_test

import (
	"encoding/binary"
	"errors"
	"os"
	"testing"

	ext4 "github.com/go-filesystems/ext4"
)

func newTempFS(t *testing.T) (*ext4.Ext4FS, func()) {
	t.Helper()
	f, err := os.CreateTemp("", "ext4test")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	f.Close()
	fsi, err := ext4.Format(path, 4096*128, ext4.FormatConfig{})
	if err != nil {
		os.Remove(path)
		t.Fatal(err)
	}
	fs, ok := fsi.(*ext4.Ext4FS)
	if !ok {
		os.Remove(path)
		t.Fatal("Format returned non-ext4 implementation")
	}
	return fs, func() { fs.Close(); os.Remove(path) }
}

func TestReadWriteRawBlockErrors(t *testing.T) {
	rw := &errRW{readErr: errors.New("boom"), writeErr: errors.New("boom")}
	sb := &ext4.Superblock{BlockSize: 4096}

	if _, err := ext4.ReadRawBlock(rw, 0, sb, 1); err == nil {
		t.Fatalf("expected readRawBlock to fail")
	}
	if err := ext4.WriteRawBlock(rw, 0, sb, 1, []byte{1, 2, 3}); err == nil {
		t.Fatalf("expected writeRawBlock to fail")
	}
}

func TestReadInodeZeroAndAllocBlocksFail(t *testing.T) {
	fs, cleanup := newTempFS(t)
	defer cleanup()

	rw := ext4.CloneFSImage(t, fs)
	sb := ext4.CloneSuperblockFromFS(fs)

	if _, err := ext4.ReadInode(rw, 0, sb, 0); err == nil {
		t.Fatalf("expected readInode(0) to fail")
	}

	// Request more blocks than fit in a group to force an error.
	_, err := ext4.AllocBlocks(rw, 0, sb, sb.BlocksPerGroup+1)
	if err == nil {
		t.Fatalf("expected allocBlocks to fail for oversized request")
	}
}

func TestFreeBlockEarlyReturn(t *testing.T) {
	fs, cleanup := newTempFS(t)
	defer cleanup()
	// Force FirstDataBlock > 0 so metadata blocks (0) are considered metadata.
	rw := ext4.CloneFSImage(t, fs)
	sb := ext4.CloneSuperblockFromFS(fs)
	sb.FirstDataBlock = 1
	if err := ext4.FreeBlock(rw, 0, sb, 0); err != nil {
		t.Fatalf("freeBlock for metadata should be no-op: %v", err)
	}
}

func TestWriteBitmapAndBGDMetadataCsum(t *testing.T) {
	fs, cleanup := newTempFS(t)
	defer cleanup()
	// Enable metadata csum code paths and a large descriptor size to hit hi/lo branches.
	rw := ext4.CloneFSImage(t, fs)
	sb := ext4.CloneSuperblockFromFS(fs)
	sb.FeatureROCompat |= ext4.FeatROCompatMetadataCsum
	sb.DescSize = 64

	oldD, err := ext4.ReadBGD(rw, 0, sb, 0)
	if err != nil {
		t.Fatalf("readBGD: %v", err)
	}
	// Build a decoded descriptor buffer sized to sb.DescSize and copy fields
	// from the decoded BGD so we can exercise 64-byte descriptor paths.
	dRaw := make([]byte, sb.DescSize)
	d := ext4.DecodeBGD(dRaw, sb)
	d.BlockBitmapBlock = oldD.BlockBitmapBlock
	d.InodeBitmapBlock = oldD.InodeBitmapBlock
	d.InodeTableBlock = oldD.InodeTableBlock
	d.FreeBlocksCount = oldD.FreeBlocksCount
	d.FreeInodesCount = oldD.FreeInodesCount
	d.UsedDirsCount = oldD.UsedDirsCount
	d.Flags = oldD.Flags

	// exercise writeBitmapWithCsum for block and inode bitmaps
	if err := ext4.WriteBitmapWithCsum(rw, 0, sb, 0, d, true); err != nil {
		t.Fatalf("writeBitmapWithCsum(block): %v", err)
	}
	if err := ext4.WriteBitmapWithCsum(rw, 0, sb, 0, d, false); err != nil {
		t.Fatalf("writeBitmapWithCsum(inode): %v", err)
	}

	// exercise writeBGD metadata csum path
	if err := ext4.WriteBGD(rw, 0, sb, 0, d); err != nil {
		t.Fatalf("writeBGD: %v", err)
	}
}

func TestDecodeBGD64AndSuperblockCsum(t *testing.T) {
	fs, cleanup := newTempFS(t)
	defer cleanup()
	// Craft a 64-byte raw descriptor with hi parts set.
	raw := make([]byte, 64)
	le := binary.LittleEndian
	le.PutUint32(raw[0:], 0x11223344)  // blo
	le.PutUint32(raw[32:], 0x55667788) // bhi
	sb := *ext4.CloneSuperblockFromFS(fs)
	sb.FeatureIncompat |= ext4.FeatIncompat64bit
	sb.DescSize = 64

	d := ext4.DecodeBGD(raw, &sb)
	if d.BlockBitmapBlock == 0 {
		t.Fatalf("expected decoded block bitmap to be non-zero")
	}

	// Enable metadata csum and write the superblock to hit checksum branch.
	rw := ext4.CloneFSImage(t, fs)
	sb2 := ext4.CloneSuperblockFromFS(fs)
	sb2.FeatureROCompat |= ext4.FeatROCompatMetadataCsum
	if err := ext4.WriteSuperblock(rw, 0, sb2); err != nil {
		t.Fatalf("writeSuperblock: %v", err)
	}
}

func TestExtentParsingAndInlineExtentsErrors(t *testing.T) {
	fs, cleanup := newTempFS(t)
	defer cleanup()
	// parseExtentNode should fail on bad magic
	rw := ext4.CloneFSImage(t, fs)
	sb := ext4.CloneSuperblockFromFS(fs)
	if _, err := ext4.ParseExtentNode(rw, 0, sb, make([]byte, 60), 2, nil); err == nil {
		t.Fatalf("expected parseExtentNode to fail on bad magic")
	}

	// setInlineExtents with too many extents
	in := ext4.NewTestInode(42, uint16(sb.InodeSize))
	var exts []ext4.ExtentLeaf
	for i := 0; i < 5; i++ {
		exts = append(exts, ext4.ExtentLeaf{LogBlock: uint32(i), PhysBlock: uint64(i), Count: 1})
	}
	if err := ext4.SetInlineExtents(in, exts); err == nil {
		t.Fatalf("expected setInlineExtents to fail when >4 extents")
	}
}

func TestFreeInodeBlocksOldStyleAndReadFileDataAndSymlink(t *testing.T) {
	fs, cleanup := newTempFS(t)
	defer cleanup()
	rw := ext4.CloneFSImage(t, fs)
	sb := ext4.CloneSuperblockFromFS(fs)

	// Old-style block map (no extents) should be a no-op.
	in := ext4.NewTestInode(100, uint16(sb.InodeSize))
	if err := ext4.FreeInodeBlocks(rw, 0, sb, in); err != nil {
		t.Fatalf("freeInodeBlocks old-style: %v", err)
	}

	// Allocate a block, write data, create an inline-extent inode, read it back.
	phys, err := ext4.AllocBlocks(rw, 0, sb, 1)
	if err != nil {
		t.Fatalf("allocBlocks: %v", err)
	}
	data := []byte("hello ext4 coverage test")
	buf := make([]byte, sb.BlockSize)
	copy(buf, data)
	if err := ext4.WriteRawBlock(rw, 0, sb, phys[0], buf); err != nil {
		t.Fatalf("writeRawBlock: %v", err)
	}

	inoNum, err := ext4.AllocInode(rw, 0, sb, false)
	if err != nil {
		t.Fatalf("allocInode: %v", err)
	}
	in2 := ext4.NewTestInode(inoNum, uint16(sb.InodeSize))
	if err := ext4.SetInlineExtents(in2, []ext4.ExtentLeaf{{LogBlock: 0, PhysBlock: phys[0], Count: 1}}); err != nil {
		t.Fatalf("setInlineExtents: %v", err)
	}
	ext4.SetSize(in2, uint64(len(data)))
	if err := ext4.WriteInode(rw, 0, sb, in2); err != nil {
		t.Fatalf("writeInode: %v", err)
	}

	got, err := ext4.ReadFileData(rw, 0, sb, in2)
	if err != nil {
		t.Fatalf("readFileData: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("readFileData mismatch: %q != %q", string(got), string(data))
	}

	// Slow symlink: create a symlink inode that points to data stored in blocks (>60 bytes)
	longTarget := make([]byte, 100)
	for i := range longTarget {
		longTarget[i] = 'a' + byte(i%26)
	}
	// allocate a block and write the long target
	phys2, err := ext4.AllocBlocks(rw, 0, sb, 1)
	if err != nil {
		t.Fatalf("allocBlocks symlink: %v", err)
	}
	buf2 := make([]byte, sb.BlockSize)
	copy(buf2, longTarget)
	if err := ext4.WriteRawBlock(rw, 0, sb, phys2[0], buf2); err != nil {
		t.Fatalf("writeRawBlock symlink: %v", err)
	}

	syInoNum, err := ext4.AllocInode(rw, 0, sb, false)
	if err != nil {
		t.Fatalf("allocInode symlink: %v", err)
	}
	sin := ext4.NewTestInode(syInoNum, uint16(sb.InodeSize))
	if err := ext4.SetInlineExtents(sin, []ext4.ExtentLeaf{{LogBlock: 0, PhysBlock: phys2[0], Count: 1}}); err != nil {
		t.Fatalf("setInlineExtents symlink: %v", err)
	}
	ext4.SetSize(sin, uint64(len(longTarget)))
	ext4.SetMode(sin, 0xA000, 1) // symlink
	if err := ext4.WriteInode(rw, 0, sb, sin); err != nil {
		t.Fatalf("writeInode symlink: %v", err)
	}

	out, err := ext4.ReadSymlink(rw, 0, sb, sin)
	if err != nil {
		t.Fatalf("readSymlink: %v", err)
	}
	if out != string(longTarget) {
		t.Fatalf("symlink mismatch")
	}
}

func TestWriteReadDeleteFileAndRenameCrossParent(t *testing.T) {
	fs, cleanup := newTempFS(t)
	defer cleanup()

	// Write a file and read it back.
	data := []byte("file content")
	if err := fs.WriteFile("/foo", data, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := fs.ReadFile("/foo")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("content mismatch")
	}

	// Overwrite the file (exercise existingIno branch)
	data2 := []byte("new content longer than before to exercise freeing")
	if err := fs.WriteFile("/foo", data2, 0644); err != nil {
		t.Fatalf("WriteFile overwrite: %v", err)
	}

	// Delete the file
	if err := fs.DeleteFile("/foo"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}

	// Create directories and test cross-parent rename updating ".."
	if err := fs.MkDir("/a", 0755); err != nil {
		t.Fatalf("MkDir /a: %v", err)
	}
	if err := fs.MkDir("/a/dir", 0755); err != nil {
		t.Fatalf("MkDir /a/dir: %v", err)
	}
	if err := fs.MkDir("/b", 0755); err != nil {
		t.Fatalf("MkDir /b: %v", err)
	}

	if err := fs.Rename("/a/dir", "/b/dir"); err != nil {
		t.Fatalf("Rename cross-parent: %v", err)
	}
}

func TestAddDirEntryAllocatesNewBlock(t *testing.T) {
	fs, cleanup := newTempFS(t)
	defer cleanup()
	// Fill the root directory's first block with minimal entries (no slack)
	rw := ext4.CloneFSImage(t, fs)
	sb := ext4.CloneSuperblockFromFS(fs)

	rootIno, err := ext4.ReadInode(rw, 0, sb, ext4.RootIno)
	if err != nil {
		t.Fatalf("read root inode: %v", err)
	}
	rootBlock := ext4.DirBlockForPath(t, rw, sb, "/")

	// Build a directory block packed with entries of rec_len == MinDirentSize (no slack)
	buf := make([]byte, sb.BlockSize)
	off := 0
	ino := uint32(11)
	for off+8 <= int(sb.BlockSize)-12 {
		le := binary.LittleEndian
		le.PutUint32(buf[off:], ino)
		le.PutUint16(buf[off+4:], uint16(ext4.MinDirentSize(1)))
		buf[off+6] = 1
		buf[off+7] = ext4.FtRegFile
		buf[off+8] = 'x'
		off += ext4.MinDirentSize(1)
		ino++
	}
	// Tail entry
	tailOff := int(sb.BlockSize) - 12
	le := binary.LittleEndian
	le.PutUint32(buf[tailOff:], 0)
	le.PutUint16(buf[tailOff+4:], 12)
	buf[tailOff+6] = 0
	buf[tailOff+7] = ext4.FtDirTail

	if err := ext4.WriteRawBlock(rw, 0, sb, rootBlock, buf); err != nil {
		t.Fatalf("writeRawBlock root: %v", err)
	}

	// Allocate a child inode and call addDirEntry — this should allocate a new block
	child, err := ext4.AllocInode(rw, 0, sb, false)
	if err != nil {
		t.Fatalf("allocInode child: %v", err)
	}
	if err := ext4.AddDirEntry(rw, 0, sb, rootIno, child, "zz", ext4.FtRegFile); err != nil {
		t.Fatalf("addDirEntry allocate: %v", err)
	}
}

func TestFSReadLinkErrors(t *testing.T) {
	fs, cleanup := newTempFS(t)
	defer cleanup()

	// Create a regular file and verify ReadLink returns error for non-symlinks.
	data := []byte("ordinary file")
	if err := fs.WriteFile("/foo", data, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := fs.ReadLink("/foo"); err == nil {
		t.Fatalf("expected ReadLink to fail for non-symlink")
	}
}

func TestAllocInodeDirAndAllocFreeBlocks(t *testing.T) {
	fs, cleanup := newTempFS(t)
	defer cleanup()
	// Operate on an in-memory clone so we can use exported wrappers.
	rw := ext4.CloneFSImage(t, fs)
	sb := ext4.CloneSuperblockFromFS(fs)

	ino, err := ext4.AllocInode(rw, 0, sb, true)
	if err != nil {
		t.Fatalf("allocInode(dir): %v", err)
	}
	if ino == 0 {
		t.Fatalf("allocInode returned 0")
	}

	phys, err := ext4.AllocBlocks(rw, 0, sb, 2)
	if err != nil {
		t.Fatalf("allocBlocks 2: %v", err)
	}
	if len(phys) != 2 {
		t.Fatalf("expected 2 blocks")
	}
	for _, b := range phys {
		if err := ext4.FreeBlock(rw, 0, sb, b); err != nil {
			t.Fatalf("freeBlock: %v", err)
		}
	}
}

func TestFreeInodeSlotAndRemoveFileIdempotent(t *testing.T) {
	fs, cleanup := newTempFS(t)
	defer cleanup()
	rw := ext4.CloneFSImage(t, fs)
	sb := ext4.CloneSuperblockFromFS(fs)

	ino, err := ext4.AllocInode(rw, 0, sb, false)
	if err != nil {
		t.Fatalf("allocInode: %v", err)
	}
	if err := ext4.FreeInodeSlot(rw, 0, sb, ino); err != nil {
		t.Fatalf("freeInodeSlot: %v", err)
	}

	// removing a missing file should be idempotent (nil)
	if err := ext4.RemoveFile(rw, 0, sb, "/i-do-not-exist"); err != nil {
		t.Fatalf("removeFile idempotent: %v", err)
	}
}

func TestZeroEntryInBlockBehavior(t *testing.T) {
	// Create a dir block with a single entry "abc" and test zeroing
	sb := &ext4.Superblock{BlockSize: 1024}
	buf := make([]byte, sb.BlockSize)
	le := binary.LittleEndian
	le.PutUint32(buf[0:], 123) // ino
	le.PutUint16(buf[4:], uint16(ext4.MinDirentSize(3)))
	buf[0+6] = 3
	buf[0+7] = ext4.FtRegFile
	copy(buf[8:], "abc")

	if !ext4.ZeroEntryInBlock(buf, "abc", binary.LittleEndian) {
		t.Fatalf("expected zeroEntryInBlock to find and zero entry")
	}
	if ext4.ZeroEntryInBlock(buf, "zzz", binary.LittleEndian) {
		t.Fatalf("expected zeroEntryInBlock to return false for missing name")
	}
}

func TestDecrementAndFreeInode_LinkCount(t *testing.T) {
	fs, cleanup := newTempFS(t)
	defer cleanup()
	rw := ext4.CloneFSImage(t, fs)
	sb := ext4.CloneSuperblockFromFS(fs)
	ino, err := ext4.AllocInode(rw, 0, sb, false)
	if err != nil {
		t.Fatalf("allocInode: %v", err)
	}
	in, err := ext4.ReadInode(rw, 0, sb, ino)
	if err != nil {
		t.Fatalf("readInode: %v", err)
	}
	// Set links to 2 and write
	le := binary.LittleEndian
	le.PutUint16(ext4.InodeRaw(in)[ext4.InodeOffLinksCount:], 2)
	if err := ext4.WriteInode(rw, 0, sb, in); err != nil {
		t.Fatalf("writeInode set links: %v", err)
	}

	if err := ext4.DecrementAndFreeInode(rw, 0, sb, in); err != nil {
		t.Fatalf("decrementAndFreeInode: %v", err)
	}
}

func TestWriteInodeWithCsum(t *testing.T) {
	fs, cleanup := newTempFS(t)
	defer cleanup()
	// enable metadata csum so writeInode calls computeInodeCsum
	rw := ext4.CloneFSImage(t, fs)
	sb := ext4.CloneSuperblockFromFS(fs)
	sb.FeatureROCompat |= ext4.FeatROCompatMetadataCsum
	if err := ext4.WriteSuperblock(rw, 0, sb); err != nil {
		t.Fatalf("writeSuperblock: %v", err)
	}
	ino, err := ext4.AllocInode(rw, 0, sb, false)
	if err != nil {
		t.Fatalf("allocInode: %v", err)
	}
	in, err := ext4.ReadInode(rw, 0, sb, ino)
	if err != nil {
		t.Fatalf("readInode: %v", err)
	}
	if err := ext4.WriteInode(rw, 0, sb, in); err != nil {
		t.Fatalf("writeInode csum: %v", err)
	}
}

func TestLookupPathFromLoop(t *testing.T) {
	fs, cleanup := newTempFS(t)
	defer cleanup()
	rw := ext4.CloneFSImage(t, fs)
	sb := ext4.CloneSuperblockFromFS(fs)
	if _, err := ext4.LookupPath(rw, 0, sb, "/some/path"); err == nil {
		t.Fatalf("expected symlink loop error")
	}
}

func TestTryInsertDirEntrySuccess(t *testing.T) {
	sb := &ext4.Superblock{BlockSize: 1024}
	buf := make([]byte, sb.BlockSize)
	le := binary.LittleEndian
	// one large entry with slack
	le.PutUint32(buf[0:], 2)
	le.PutUint16(buf[4:], uint16(sb.BlockSize-12))
	buf[6] = 1
	buf[7] = ext4.FtRegFile
	copy(buf[8:], "a")
	// tail entry
	tailOff := int(sb.BlockSize) - 12
	le.PutUint32(buf[tailOff:], 0)
	le.PutUint16(buf[tailOff+4:], 12)
	buf[tailOff+6] = 0
	buf[tailOff+7] = ext4.FtDirTail

	if !ext4.TryInsertDirEntry(buf, 123, "zz", ext4.FtRegFile, ext4.MinDirentSize(2)) {
		t.Fatalf("tryInsertDirEntry expected to succeed")
	}
}

func TestRemoveFileNonRegular(t *testing.T) {
	fs, cleanup := newTempFS(t)
	defer cleanup()
	if err := fs.MkDir("/d", 0755); err != nil {
		t.Fatalf("MkDir /d: %v", err)
	}
	// removeFile should return an error for non-regular file
	rw := ext4.CloneFSImage(t, fs)
	sb := ext4.CloneSuperblockFromFS(fs)
	if err := ext4.RemoveFile(rw, 0, sb, "/d"); err == nil {
		t.Fatalf("expected removeFile to error for non-regular file")
	}
}

func TestRenameNoOpAndTypeMismatch(t *testing.T) {
	fs, cleanup := newTempFS(t)
	defer cleanup()
	if err := fs.WriteFile("/foo", []byte("x"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := fs.MkDir("/dir", 0755); err != nil {
		t.Fatalf("MkDir: %v", err)
	}

	// no-op rename
	if err := fs.Rename("/foo", "/foo"); err != nil {
		t.Fatalf("Rename no-op: %v", err)
	}

	// rename file onto directory should fail
	if err := fs.Rename("/foo", "/dir"); err == nil {
		t.Fatalf("expected rename to fail replacing directory with non-directory")
	}
}

func TestLookupParentTrailingSlash(t *testing.T) {
	fs, cleanup := newTempFS(t)
	defer cleanup()
	rw := ext4.CloneFSImage(t, fs)
	sb := ext4.CloneSuperblockFromFS(fs)
	if _, _, err := ext4.LookupParent(rw, 0, sb, "/a/"); err == nil {
		t.Fatalf("expected lookupParent to reject trailing slash")
	}
}

func TestParseExtentNodeIndex(t *testing.T) {
	fs, cleanup := newTempFS(t)
	defer cleanup()

	// allocate a child block to hold a leaf extent
	rw := ext4.CloneFSImage(t, fs)
	sb := ext4.CloneSuperblockFromFS(fs)
	phys, err := ext4.AllocBlocks(rw, 0, sb, 1)
	if err != nil {
		t.Fatalf("allocBlocks: %v", err)
	}
	child := phys[0]
	// build a child block (leaf) with one extent
	childBuf := make([]byte, sb.BlockSize)
	le := binary.LittleEndian
	le.PutUint16(childBuf[0:], ext4.ExtentMagic)
	le.PutUint16(childBuf[2:], 1)
	le.PutUint16(childBuf[6:], 0)
	// leaf entry at offset 12
	off := 12
	le.PutUint32(childBuf[off:], 0) // log block
	le.PutUint16(childBuf[off+4:], 1)
	le.PutUint16(childBuf[off+6:], uint16(child>>32))
	le.PutUint32(childBuf[off+8:], uint32(child&0xFFFFFFFF))
	if err := ext4.WriteRawBlock(rw, 0, sb, child, childBuf); err != nil {
		t.Fatalf("writeRawBlock child: %v", err)
	}

	// parent 60-byte buffer: depth=1, one index entry pointing to child
	parentBuf := make([]byte, 60)
	le.PutUint16(parentBuf[0:], ext4.ExtentMagic)
	le.PutUint16(parentBuf[2:], 1)
	le.PutUint16(parentBuf[6:], 1)
	entOff := 12
	le.PutUint32(parentBuf[entOff:], 0)
	le.PutUint32(parentBuf[entOff+4:], uint32(child&0xFFFFFFFF))
	le.PutUint16(parentBuf[entOff+8:], uint16(child>>32))

	leaves, err := ext4.ParseExtentNode(rw, 0, sb, parentBuf, 42, nil)
	if err != nil {
		t.Fatalf("parseExtentNode index: %v", err)
	}
	if len(leaves) == 0 {
		t.Fatalf("expected leaves from index parse")
	}
}

func TestAllocInodeNoFree(t *testing.T) {
	fs, cleanup := newTempFS(t)
	defer cleanup()
	// Operate on clone image and use exported wrappers.
	rw := ext4.CloneFSImage(t, fs)
	sb := ext4.CloneSuperblockFromFS(fs)

	// compute number of block groups from superblock fields
	n := uint32((sb.BlocksCount + uint64(sb.BlocksPerGroup) - 1) / uint64(sb.BlocksPerGroup))
	for g := uint32(0); g < n; g++ {
		d, err := ext4.ReadBGD(rw, 0, sb, g)
		if err != nil {
			t.Fatalf("readBGD: %v", err)
		}
		// Mark all inode bits as allocated so the commit worker's
		// authoritative recompute will reflect zero free inodes.
		imap := make([]byte, sb.BlockSize)
		for i := range imap {
			imap[i] = 0xFF
		}
		if err := ext4.WriteBitmapBuf(rw, 0, sb, g, d, false, d.InodeBitmapBlock, imap); err != nil {
			t.Fatalf("write inode bitmap: %v", err)
		}
		d.FreeInodesCount = 0
		if err := ext4.WriteBGD(rw, 0, sb, g, d); err != nil {
			t.Fatalf("writeBGD: %v", err)
		}
	}
	sb.FreeInodesCount = 0
	if err := ext4.WriteSuperblock(rw, 0, sb); err != nil {
		t.Fatalf("writeSuperblock: %v", err)
	}

	if _, err := ext4.AllocInode(rw, 0, sb, false); err == nil {
		t.Fatalf("expected allocInode to fail when no free inodes")
	}
}

func TestAllocBlocksNoFree(t *testing.T) {
	fs, cleanup := newTempFS(t)
	defer cleanup()
	// Operate on clone image and use exported wrappers.
	rw := ext4.CloneFSImage(t, fs)
	sb := ext4.CloneSuperblockFromFS(fs)

	n := uint32((sb.BlocksCount + uint64(sb.BlocksPerGroup) - 1) / uint64(sb.BlocksPerGroup))
	for g := uint32(0); g < n; g++ {
		d, err := ext4.ReadBGD(rw, 0, sb, g)
		if err != nil {
			t.Fatalf("readBGD: %v", err)
		}
		// Mark all block bits as allocated so the commit worker's
		// authoritative recompute will reflect zero free blocks.
		bmap := make([]byte, sb.BlockSize)
		for i := range bmap {
			bmap[i] = 0xFF
		}
		if err := ext4.WriteBitmapBuf(rw, 0, sb, g, d, true, d.BlockBitmapBlock, bmap); err != nil {
			t.Fatalf("write block bitmap: %v", err)
		}
		d.FreeBlocksCount = 0
		if err := ext4.WriteBGD(rw, 0, sb, g, d); err != nil {
			t.Fatalf("writeBGD: %v", err)
		}
	}
	sb.FreeBlocksCount = 0
	if err := ext4.WriteSuperblock(rw, 0, sb); err != nil {
		t.Fatalf("writeSuperblock: %v", err)
	}

	if _, err := ext4.AllocBlocks(rw, 0, sb, 1); err == nil {
		t.Fatalf("expected allocBlocks to fail when no free blocks")
	}
}

func TestLookupPathFrom_SymlinkAbsoluteAndRelative(t *testing.T) {
	fs, cleanup := newTempFS(t)
	defer cleanup()
	// create a target file /target
	if err := fs.WriteFile("/target", []byte("data"), 0644); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	rw := ext4.CloneFSImage(t, fs)
	sb := ext4.CloneSuperblockFromFS(fs)

	targetIno, err := ext4.LookupPath(rw, 0, sb, "/target")
	if err != nil {
		t.Fatalf("lookupPath target: %v", err)
	}

	// create a fast (inline) symlink /link -> /target
	linkInoNum, err := ext4.AllocInode(rw, 0, sb, false)
	if err != nil {
		t.Fatalf("allocInode for symlink: %v", err)
	}
	linkIn := ext4.NewTestInode(linkInoNum, uint16(sb.InodeSize))
	targetStr := "/target"
	copy(ext4.InodeRaw(linkIn)[ext4.InodeOffBlock:ext4.InodeOffBlock+len(targetStr)], []byte(targetStr))
	ext4.SetMode(linkIn, 0xA000, 1)
	ext4.SetSize(linkIn, uint64(len(targetStr)))
	if err := ext4.WriteInode(rw, 0, sb, linkIn); err != nil {
		t.Fatalf("writeInode symlink: %v", err)
	}
	rootIno, err := ext4.ReadInode(rw, 0, sb, ext4.RootIno)
	if err != nil {
		t.Fatalf("read root inode: %v", err)
	}
	if err := ext4.AddDirEntry(rw, 0, sb, rootIno, linkInoNum, "link", ext4.FtSymlink); err != nil {
		t.Fatalf("addDirEntry symlink: %v", err)
	}

	// lookup /link should follow to /target
	got, err := ext4.LookupPath(rw, 0, sb, "/link")
	if err != nil {
		t.Fatalf("lookupPath /link: %v", err)
	}
	if ext4.InodeNum(got) != ext4.InodeNum(targetIno) {
		t.Fatalf("symlink resolve failed: got inode %d want %d", ext4.InodeNum(got), ext4.InodeNum(targetIno))
	}

	// Now test a relative symlink: create /d and /d/tgt and symlink /d/s -> tgt
	if err := fs.MkDir("/d", 0755); err != nil {
		t.Fatalf("MkDir /d: %v", err)
	}
	if err := fs.WriteFile("/d/tgt", []byte("x"), 0644); err != nil {
		t.Fatalf("WriteFile /d/tgt: %v", err)
	}
	// refresh clone
	rw = ext4.CloneFSImage(t, fs)
	sb = ext4.CloneSuperblockFromFS(fs)
	tgtIno, err := ext4.LookupPath(rw, 0, sb, "/d/tgt")
	if err != nil {
		t.Fatalf("lookupPath /d/tgt: %v", err)
	}

	sino, err := ext4.AllocInode(rw, 0, sb, false)
	if err != nil {
		t.Fatalf("allocInode rel symlink: %v", err)
	}
	sin := ext4.NewTestInode(sino, uint16(sb.InodeSize))
	rel := "tgt"
	copy(ext4.InodeRaw(sin)[ext4.InodeOffBlock:ext4.InodeOffBlock+len(rel)], []byte(rel))
	ext4.SetMode(sin, 0xA000, 1)
	ext4.SetSize(sin, uint64(len(rel)))
	if err := ext4.WriteInode(rw, 0, sb, sin); err != nil {
		t.Fatalf("writeInode rel symlink: %v", err)
	}
	din, err := ext4.LookupPath(rw, 0, sb, "/d")
	if err != nil {
		t.Fatalf("lookupPath /d: %v", err)
	}
	// add entry into /d
	if err := ext4.AddDirEntry(rw, 0, sb, din, sino, "s", ext4.FtSymlink); err != nil {
		t.Fatalf("addDirEntry /d/s: %v", err)
	}

	got2, err := ext4.LookupPath(rw, 0, sb, "/d/s")
	if err != nil {
		t.Fatalf("lookupPath /d/s: %v", err)
	}
	if ext4.InodeNum(got2) != ext4.InodeNum(tgtIno) {
		t.Fatalf("relative symlink resolve failed: got %d want %d", ext4.InodeNum(got2), ext4.InodeNum(tgtIno))
	}
}

func TestRename_ReplaceFileAndReplaceEmptyDir(t *testing.T) {
	fs, cleanup := newTempFS(t)
	defer cleanup()

	// prepare directories
	if err := fs.MkDir("/a", 0755); err != nil {
		t.Fatalf("MkDir /a: %v", err)
	}
	if err := fs.MkDir("/b", 0755); err != nil {
		t.Fatalf("MkDir /b: %v", err)
	}

	// create file foo in /a and bar in /b
	if err := fs.WriteFile("/a/foo", []byte("A"), 0644); err != nil {
		t.Fatalf("WriteFile /a/foo: %v", err)
	}
	if err := fs.WriteFile("/b/bar", []byte("B"), 0644); err != nil {
		t.Fatalf("WriteFile /b/bar: %v", err)
	}

	// rename /a/foo -> /b/bar (replace existing file)
	if err := fs.Rename("/a/foo", "/b/bar"); err != nil {
		t.Fatalf("Rename replace file: %v", err)
	}
	// target should now contain content from /a/foo
	got, err := fs.ReadFile("/b/bar")
	if err != nil {
		t.Fatalf("ReadFile /b/bar: %v", err)
	}
	if string(got) != "A" {
		t.Fatalf("rename didn't move file content")
	}

	// now test replacing empty directory
	if err := fs.MkDir("/c", 0755); err != nil {
		t.Fatalf("MkDir /c: %v", err)
	}
	if err := fs.MkDir("/c/old", 0755); err != nil {
		t.Fatalf("MkDir /c/old: %v", err)
	}
	if err := fs.MkDir("/d", 0755); err != nil {
		t.Fatalf("MkDir /d: %v", err)
	}
	if err := fs.MkDir("/d/old", 0755); err != nil {
		t.Fatalf("MkDir /d/old: %v", err)
	}

	if err := fs.Rename("/c/old", "/d/new"); err != nil {
		t.Fatalf("Rename move dir across parents: %v", err)
	}
	if _, err := fs.Stat("/d/new"); err != nil {
		t.Fatalf("Stat after rename expected /d/new present: %v", err)
	}
	if _, err := fs.Stat("/c/old"); err == nil {
		t.Fatalf("expected /c/old to be gone")
	}
}

func TestReadBGDAndWriteBGDErrorPaths(t *testing.T) {
	fs, cleanup := newTempFS(t)
	defer cleanup()

	rw := &errRW{readErr: errors.New("boom"), writeErr: errors.New("boom")}
	sb := ext4.CloneSuperblockFromFS(fs)
	if _, err := ext4.ReadBGD(rw, 0, sb, 0); err == nil {
		t.Fatalf("expected readBGD to fail on read error")
	}
	d := ext4.DecodeBGD(make([]byte, sb.DescSize), sb)
	if err := ext4.WriteBGD(rw, 0, sb, 0, d); err == nil {
		t.Fatalf("expected writeBGD to fail on write error")
	}
}

func TestReadInodeAndSuperblockWriteErrors(t *testing.T) {
	fs, cleanup := newTempFS(t)
	defer cleanup()
	rw := &errRW{readErr: errors.New("boom"), writeErr: errors.New("boom")}
	sb := ext4.CloneSuperblockFromFS(fs)
	if _, err := ext4.ReadInode(rw, 0, sb, 2); err == nil {
		t.Fatalf("expected readInode to fail on read error")
	}
	if err := ext4.WriteSuperblock(rw, 0, sb); err == nil {
		t.Fatalf("expected writeSuperblock to fail on write error")
	}
}

func TestInodeExtentsErrors(t *testing.T) {
	fs, cleanup := newTempFS(t)
	defer cleanup()
	rw := ext4.CloneFSImage(t, fs)
	sb := ext4.CloneSuperblockFromFS(fs)
	in := ext4.NewTestInode(5, uint16(sb.InodeSize))
	ext4.SetMode(in, 0x8000, 1)
	le := binary.LittleEndian

	// Inline data is now supported on read: an empty inline inode reads as
	// empty content. Persist the inode so readFileData's on-disk re-read
	// observes the inline flag.
	le.PutUint32(ext4.InodeRaw(in)[ext4.InodeOffFlags:], uint32(ext4.InodeFlagInlineData))
	if err := ext4.WriteInode(rw, 0, sb, in); err != nil {
		t.Fatalf("WriteInode (inline): %v", err)
	}
	if d, err := ext4.ReadFileData(rw, 0, sb, in); err != nil {
		t.Fatalf("readFileData on inline inode: %v", err)
	} else if len(d) != 0 {
		t.Fatalf("expected empty inline content, got %d bytes", len(d))
	}

	// Old-style block map (extents flag clear) is now supported on read paths;
	// an empty (size 0) block-map inode yields empty content, not an error.
	le.PutUint32(ext4.InodeRaw(in)[ext4.InodeOffFlags:], 0)
	if err := ext4.WriteInode(rw, 0, sb, in); err != nil {
		t.Fatalf("WriteInode (block map): %v", err)
	}
	data, err := ext4.ReadFileData(rw, 0, sb, in)
	if err != nil {
		t.Fatalf("readFileData on block-map inode: %v", err)
	}
	if len(data) != 0 {
		t.Fatalf("expected empty content, got %d bytes", len(data))
	}
}

func TestAllocInodeAcrossGroups(t *testing.T) {
	// Create a multi-group filesystem to exercise group iteration in allocInode.
	path := t.TempDir() + "/big.img"
	fsi, err := ext4.Format(path, 256*1024*1024, ext4.FormatConfig{})
	if err != nil {
		t.Fatalf("Format multi-group: %v", err)
	}
	fs, ok := fsi.(*ext4.Ext4FS)
	if !ok {
		t.Fatalf("Format returned non-ext4 implementation")
	}
	defer fs.Close()

	// Zero free inodes in group 0 so allocInode must consider later groups.
	rw := ext4.CloneFSImage(t, fs)
	sb := ext4.CloneSuperblockFromFS(fs)
	n := uint32((sb.BlocksCount + uint64(sb.BlocksPerGroup) - 1) / uint64(sb.BlocksPerGroup))
	if n < 2 {
		t.Skip("expected multiple groups for this test")
	}
	if d, err := ext4.ReadBGD(rw, 0, sb, 0); err == nil {
		d.FreeInodesCount = 0
		if err := ext4.WriteBGD(rw, 0, sb, 0, d); err != nil {
			t.Fatalf("writeBGD: %v", err)
		}
	}

	ino, err := ext4.AllocInode(rw, 0, sb, false)
	if err != nil {
		t.Fatalf("allocInode across groups: %v", err)
	}
	if ino == 0 {
		t.Fatalf("allocInode returned 0")
	}
}

func TestFreeInodeSlotWithDesc64(t *testing.T) {
	fs, cleanup := newTempFS(t)
	defer cleanup()
	// Enable metadata csum and large descriptor to hit hi-word branches.
	rw := ext4.CloneFSImage(t, fs)
	sb := ext4.CloneSuperblockFromFS(fs)
	sb.FeatureROCompat |= ext4.FeatROCompatMetadataCsum
	sb.DescSize = 64

	ino, err := ext4.AllocInode(rw, 0, sb, false)
	if err != nil {
		t.Fatalf("allocInode: %v", err)
	}
	if err := ext4.FreeInodeSlot(rw, 0, sb, ino); err != nil {
		t.Fatalf("freeInodeSlot: %v", err)
	}
}
