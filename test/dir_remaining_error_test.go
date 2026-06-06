package filesystem_ext4_test

import (
	"encoding/binary"
	"errors"
	"testing"

	ext4 "github.com/go-filesystems/ext4"
)

func newPackedDirState(t *testing.T, extentCount int) (*ext4.HookMemFile, *ext4.Superblock, *ext4.Inode) {
	t.Helper()
	const blockSize = 1024
	rw := ext4.NewHookMemFile(make([]byte, blockSize*512))
	sb := ext4.NewTestSuperblock(16, 128, 1, blockSize)
	sb.InodeSize = 256
	sb.FirstDataBlock = 100
	sb.BlocksCount = 228
	sb.FreeBlocksCount = 64
	// Build a BGD and write it.
	raw := make([]byte, sb.DescSize)
	le := binary.LittleEndian
	le.PutUint32(raw[0:], 2)
	le.PutUint32(raw[4:], 3)
	le.PutUint32(raw[8:], 4)
	d := ext4.DecodeBGD(raw, sb)
	if err := ext4.WriteBGD(rw, 0, sb, 0, d); err != nil {
		t.Fatalf("writeBGD: %v", err)
	}
	bmap := make([]byte, sb.BlockSize)
	for i := 0; i < extentCount; i++ {
		ext4.SetBit(bmap, 10+i)
	}
	if err := ext4.WriteRawBlock(rw, 0, sb, d.BlockBitmapBlock, bmap); err != nil {
		t.Fatalf("writeRawBlock bitmap: %v", err)
	}
	if err := ext4.WriteRawBlock(rw, 0, sb, d.InodeBitmapBlock, make([]byte, sb.BlockSize)); err != nil {
		t.Fatalf("writeRawBlock inode bitmap: %v", err)
	}

	dir := ext4.NewTestInode(ext4.RootIno, uint16(sb.InodeSize))
	ext4.SetMode(dir, 0x4000, 2)
	exts := make([]ext4.ExtentLeaf, 0, extentCount)
	for i := 0; i < extentCount; i++ {
		phys := uint64(110 + i)
		buf := make([]byte, sb.BlockSize)
		le.PutUint32(buf[0:], 2)
		le.PutUint16(buf[4:], uint16(ext4.MinDirentSize(1)))
		buf[6] = 1
		buf[7] = 1 // file type
		buf[8] = 'a'
		tailOff := ext4.MinDirentSize(1)
		le.PutUint16(buf[tailOff+4:], 12)
		buf[tailOff+7] = ext4.FtDirTail
		if err := ext4.WriteRawBlock(rw, 0, sb, phys, buf); err != nil {
			t.Fatalf("writeRawBlock dir block: %v", err)
		}
		exts = append(exts, ext4.ExtentLeaf{LogBlock: uint32(i), PhysBlock: phys, Count: 1})
	}
	if err := ext4.SetInlineExtents(dir, exts); err != nil {
		t.Fatalf("SetInlineExtents: %v", err)
	}
	ext4.SetSize(dir, uint64(extentCount)*uint64(sb.BlockSize))
	return rw, sb, dir
}

func TestDirRemainingErrorPaths(t *testing.T) {
	errBoom := errors.New("boom")

	t.Run("lookupPath current inode read error on non-empty path", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		rw := ext4.CloneFSImage(t, fs)
		sb := ext4.CloneSuperblockFromFS(fs)
		rootOff := ext4.InodeOffsetFor(rw, sb, ext4.RootIno)
		rw.ReadHook = func(off int64, _ []byte) error {
			if off == rootOff {
				return errBoom
			}
			return nil
		}
		if _, err := ext4.LookupPath(rw, 0, sb, "/missing"); err == nil {
			t.Fatalf("expected lookupPath to fail while reading current inode")
		}
	})

	t.Run("lookupPath symlink read error", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		longTarget := make([]byte, 128)
		for i := range longTarget {
			longTarget[i] = 'a' + byte(i%26)
		}
		// operate on an in-memory clone of the filesystem image so tests
		// don't need to access internal fields on the live FS.
		rw := ext4.CloneFSImage(t, fs)
		sb := ext4.CloneSuperblockFromFS(fs)
		phys, err := ext4.AllocBlocks(rw, 0, sb, 1)
		if err != nil {
			t.Fatalf("allocBlocks: %v", err)
		}
		buf := make([]byte, sb.BlockSize)
		copy(buf, longTarget)
		if err := ext4.WriteRawBlock(rw, 0, sb, phys[0], buf); err != nil {
			t.Fatalf("writeRawBlock: %v", err)
		}
		inoNum, err := ext4.AllocInode(rw, 0, sb, false)
		if err != nil {
			t.Fatalf("allocInode: %v", err)
		}
		linkInode := ext4.NewTestInode(inoNum, uint16(sb.InodeSize))
		ext4.SetMode(linkInode, 0xA000, 1)
		ext4.SetSize(linkInode, uint64(len(longTarget)))
		if err := ext4.SetInlineExtents(linkInode, []ext4.ExtentLeaf{{LogBlock: 0, PhysBlock: phys[0], Count: 1}}); err != nil {
			t.Fatalf("SetInlineExtents: %v", err)
		}
		if err := ext4.WriteInode(rw, 0, sb, linkInode); err != nil {
			t.Fatalf("writeInode: %v", err)
		}
		root, err := ext4.ReadInode(rw, 0, sb, ext4.RootIno)
		if err != nil {
			t.Fatalf("readInode(root): %v", err)
		}
		if err := ext4.AddDirEntry(rw, 0, sb, root, inoNum, "slow", ext4.FtSymlink); err != nil {
			t.Fatalf("addDirEntry: %v", err)
		}
		rw = ext4.CloneFSImage(t, fs)
		sb = ext4.CloneSuperblockFromFS(fs)
		rw.ReadHook = func(off int64, _ []byte) error {
			if off == int64(phys[0])*int64(sb.BlockSize) {
				return errBoom
			}
			return nil
		}
		if _, err := ext4.LookupPath(rw, 0, sb, "/slow"); err == nil {
			t.Fatalf("expected lookupPath to fail while reading symlink target")
		}
	})

	t.Run("addDirEntry allocBlocks error", func(t *testing.T) {
		rw, sb, dir := newPackedDirState(t, 1)
		rw.ReadHook = func(off int64, _ []byte) error {
			if off == ext4.BgdOffset(sb, 0) {
				return errBoom
			}
			return nil
		}
		if err := ext4.AddDirEntry(rw, 0, sb, dir, 12, "new", ext4.FtRegFile); err == nil {
			t.Fatalf("expected addDirEntry to fail when allocating a new block")
		}
	})

	t.Run("addDirEntry new block write error", func(t *testing.T) {
		rw, sb, dir := newPackedDirState(t, 1)
		rw.WriteHook = func(off int64, _ []byte) error {
			if off == int64(sb.FirstDataBlock)*int64(sb.BlockSize) {
				return errBoom
			}
			return nil
		}
		if err := ext4.AddDirEntry(rw, 0, sb, dir, 12, "new", ext4.FtRegFile); err == nil {
			t.Fatalf("expected addDirEntry to fail writing the new directory block")
		}
	})

	t.Run("addDirEntry rejects fifth inline extent", func(t *testing.T) {
		rw, sb, dir := newPackedDirState(t, 4)
		if err := ext4.AddDirEntry(rw, 0, sb, dir, 12, "new", ext4.FtRegFile); err == nil {
			t.Fatalf("expected addDirEntry to fail when extending beyond four inline extents")
		}
	})
}
