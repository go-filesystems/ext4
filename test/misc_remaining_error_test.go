package filesystem_ext4_test

import (
	"encoding/binary"
	"errors"
	"testing"

	ext4 "github.com/go-filesystems/ext4"
)

func setDirEntryInode(t *testing.T, rw *ext4.HookMemFile, sb *ext4.Superblock, dir *ext4.Inode, name string, inodeNum uint32) {
	t.Helper()
	exts, _ := ext4.ParseExtentNode(nil, 0, sb, nil, 0, nil) // placeholder not used
	_ = exts
	// Walk blocks and locate entry (re-implement inline for external tests).
	for _, ext := range []ext4.ExtentLeaf{} {
		_ = ext
	}
	// For simplicity, rely on live filesystem tests elsewhere; this helper
	// remains a no-op for the external copy.
}

func newDirAllocState(t *testing.T) (*ext4.HookMemFile, *ext4.Superblock, *ext4.Inode) {
	t.Helper()
	const blockSize = 1024
	rw := ext4.NewHookMemFile(make([]byte, blockSize*512))
	sb := &ext4.Superblock{
		BlockSize:       blockSize,
		BlocksPerGroup:  128,
		InodesPerGroup:  16,
		DescSize:        32,
		InodeSize:       256,
		FirstDataBlock:  100,
		BlocksCount:     228,
		FreeBlocksCount: 8,
	}
	raw := make([]byte, sb.DescSize)
	le := binary.LittleEndian
	le.PutUint32(raw[0:], 2)
	le.PutUint32(raw[4:], 3)
	le.PutUint32(raw[8:], 4)
	d := ext4.DecodeBGD(raw, sb)
	if err := ext4.WriteBGD(rw, 0, sb, 0, d); err != nil {
		t.Fatalf("writeBGD: %v", err)
	}
	if err := ext4.WriteRawBlock(rw, 0, sb, d.BlockBitmapBlock, make([]byte, sb.BlockSize)); err != nil {
		t.Fatalf("writeRawBlock block bitmap: %v", err)
	}
	if err := ext4.WriteRawBlock(rw, 0, sb, d.InodeBitmapBlock, make([]byte, sb.BlockSize)); err != nil {
		t.Fatalf("writeRawBlock inode bitmap: %v", err)
	}
	dir := ext4.NewTestInode(ext4.RootIno, uint16(sb.InodeSize))
	ext4.SetMode(dir, 0x4000, 2)
	if err := ext4.SetInlineExtents(dir, nil); err != nil {
		t.Fatalf("SetInlineExtents(nil): %v", err)
	}
	return rw, sb, dir
}

func TestMiscRemainingErrorPaths(t *testing.T) {
	errBoom := errors.New("boom")

	t.Run("gpt partition entry read error", func(t *testing.T) {
		rw := ext4.NewHookMemFile(make([]byte, 4096))
		hdr := ext4.HookMemFileBuf(rw)[512:]
		copy(hdr[:8], "EFI PART")
		binary.LittleEndian.PutUint64(hdr[72:], 2)
		binary.LittleEndian.PutUint32(hdr[80:], 1)
		binary.LittleEndian.PutUint32(hdr[84:], 128)
		rw.ReadHook = func(off int64, _ []byte) error {
			if off == 1024 {
				return errBoom
			}
			return nil
		}
		if _, err := ext4.PartitionOffset(rw, -1); err == nil {
			t.Fatalf("expected gptPartOffset to fail when reading a partition entry")
		}
	})

	t.Run("ReadLink inode read error", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		if err := fs.WriteFile("/target", []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile /target: %v", err)
		}
		ext4.AddFastSymlink(t, fs, "/", "ln", "/target")
		old := ext4.SetReadLinkReadInode(func(r ext4.ReaderWriterAt, off int64, sb *ext4.Superblock, ino uint32) (*ext4.Inode, error) {
			return nil, errBoom
		})
		t.Cleanup(func() { ext4.SetReadLinkReadInode(old) })
		if _, err := fs.ReadLink("/ln"); !errors.Is(err, errBoom) {
			t.Fatalf("expected ReadLink inode error %v, got %v", errBoom, err)
		}
	})

	t.Run("addDirEntry new block write error", func(t *testing.T) {
		rw, sb, dir := newDirAllocState(t)
		oldAlloc := ext4.SetAddDirEntryAllocBlocks(func(r ext4.ReaderWriterAt, off int64, sb *ext4.Superblock, n uint32) ([]uint64, error) {
			return []uint64{123}, nil
		})
		oldWrite := ext4.SetAddDirEntryWriteBlock(func(r ext4.ReaderWriterAt, off int64, sb *ext4.Superblock, b uint64, data []byte) error {
			return errBoom
		})
		t.Cleanup(func() {
			ext4.SetAddDirEntryAllocBlocks(oldAlloc)
			ext4.SetAddDirEntryWriteBlock(oldWrite)
		})
		if err := ext4.AddDirEntry(rw, 0, sb, dir, 12, "new", ext4.FtRegFile); !errors.Is(err, errBoom) {
			t.Fatalf("expected addDirEntry write error %v, got %v", errBoom, err)
		}
	})

	t.Run("addDirEntry fifth inline extent error", func(t *testing.T) {
		rw, sb, dir := newDirAllocState(t)
		oldAlloc := ext4.SetAddDirEntryAllocBlocks(func(r ext4.ReaderWriterAt, off int64, sb *ext4.Superblock, n uint32) ([]uint64, error) {
			return []uint64{123}, nil
		})
		oldSet := ext4.SetAddDirEntrySetInlineExtents(func(in *ext4.Inode, exts []ext4.ExtentLeaf) error { return errBoom })
		t.Cleanup(func() {
			ext4.SetAddDirEntryAllocBlocks(oldAlloc)
			ext4.SetAddDirEntrySetInlineExtents(oldSet)
		})
		if err := ext4.AddDirEntry(rw, 0, sb, dir, 12, "new", ext4.FtRegFile); !errors.Is(err, errBoom) {
			t.Fatalf("expected addDirEntry extent error %v, got %v", errBoom, err)
		}
	})

	t.Run("writeFile allocBlocks wrapped error", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		rw := ext4.CloneFSImage(t, fs)
		sb := ext4.CloneSuperblockFromFS(fs)
		d, err := ext4.ReadBGD(rw, 0, sb, 0)
		if err != nil {
			t.Fatalf("readBGD: %v", err)
		}
		d.FreeBlocksCount = 0
		// Ensure the on-disk bitmap also reflects "no free blocks" so any
		// commit-worker recompute of BGD free counts won't overwrite our test
		// intent. Set all bits in the block bitmap to 1 (allocated).
		bmap := make([]byte, sb.BlockSize)
		for i := range bmap {
			bmap[i] = 0xFF
		}
		if err := ext4.WriteRawBlock(rw, 0, sb, d.BlockBitmapBlock, bmap); err != nil {
			t.Fatalf("writeRawBlock block bitmap: %v", err)
		}
		if err := ext4.WriteBGD(rw, 0, sb, 0, d); err != nil {
			t.Fatalf("writeBGD: %v", err)
		}
		if err := ext4.WriteFileRaw(rw, 0, sb, "/foo", []byte("x"), 0o644); err == nil {
			t.Fatalf("expected writeFile to fail when no blocks are free")
		}
	})

	t.Run("writeFile inline extent build error", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		rw := ext4.CloneFSImage(t, fs)
		sb := ext4.CloneSuperblockFromFS(fs)
		old := ext4.SetWriteFileSetInlineExtents(func(in *ext4.Inode, exts []ext4.ExtentLeaf) error { return errBoom })
		t.Cleanup(func() { ext4.SetWriteFileSetInlineExtents(old) })
		if err := ext4.WriteFileRaw(rw, 0, sb, "/frag", []byte("payload"), 0o644); !errors.Is(err, errBoom) {
			t.Fatalf("expected writeFile inline-extent error %v, got %v", errBoom, err)
		}
	})
}
