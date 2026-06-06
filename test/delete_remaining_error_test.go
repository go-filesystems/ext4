package filesystem_ext4_test

import (
	"encoding/binary"
	"errors"
	"testing"

	ext4 "github.com/go-filesystems/ext4"
)

type hookMemFile struct {
	buf       []byte
	readHook  func(off int64, p []byte) error
	writeHook func(off int64, p []byte) error
}

func (m *hookMemFile) ReadAt(p []byte, off int64) (int, error) {
	if m.readHook != nil {
		if err := m.readHook(off, p); err != nil {
			return 0, err
		}
	}
	if off < 0 || int(off) > len(m.buf) {
		return 0, errors.New("read past end")
	}
	n := copy(p, m.buf[off:])
	if n < len(p) {
		return n, errors.New("short read")
	}
	return n, nil
}

func (m *hookMemFile) WriteAt(p []byte, off int64) (int, error) {
	if m.writeHook != nil {
		if err := m.writeHook(off, p); err != nil {
			return 0, err
		}
	}
	if int(off)+len(p) > len(m.buf) {
		grow := make([]byte, int(off)+len(p))
		copy(grow, m.buf)
		m.buf = grow
	}
	n := copy(m.buf[off:], p)
	return n, nil
}

func corruptPathExtents(t *testing.T, rw ext4.ReaderWriterAt, sb *ext4.Superblock, path string) {
	t.Helper()
	in, err := ext4.LookupPath(rw, 0, sb, path)
	if err != nil {
		t.Fatalf("lookupPath(%q): %v", path, err)
	}
	buf := ext4.InodeRaw(in)
	for i := range buf[ext4.InodeOffBlock : ext4.InodeOffBlock+60] {
		buf[ext4.InodeOffBlock+i] = 0
	}
	binary.LittleEndian.PutUint32(buf[ext4.InodeOffFlags:], ext4.InodeFlagExtents)
	if err := ext4.WriteInode(rw, 0, sb, in); err != nil {
		t.Fatalf("writeInode(%q): %v", path, err)
	}
}

func newFreeInodeSlotState(t *testing.T) (*ext4.HookMemFile, *ext4.Superblock) {
	t.Helper()
	const blockSize = 4096
	rw := ext4.NewHookMemFile(make([]byte, blockSize*16))
	sb := ext4.NewTestSuperblock(16, 32, 1, blockSize)
	sb.DescSize = 32
	sb.InodeSize = 256
	sb.BlocksCount = 32
	dRaw := make([]byte, sb.DescSize)
	d := ext4.DecodeBGD(dRaw, sb)
	d.InodeBitmapBlock = 2
	d.FreeInodesCount = 8
	binary.LittleEndian.PutUint32(dRaw[4:], 2)
	if err := ext4.WriteBGD(rw, 0, sb, 0, d); err != nil {
		t.Fatalf("writeBGD: %v", err)
	}
	if err := ext4.WriteRawBlock(rw, 0, sb, d.InodeBitmapBlock, make([]byte, sb.BlockSize)); err != nil {
		t.Fatalf("writeRawBlock inode bitmap: %v", err)
	}
	return rw, sb
}

func TestDeleteRemainingErrorPaths(t *testing.T) {
	errBoom := errors.New("boom")

	t.Run("removeFile missing parent is idempotent", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		if err := fs.DeleteFile("/missing/file.txt"); err != nil {
			t.Fatalf("removeFile missing parent: %v", err)
		}
	})

	t.Run("removeDir missing parent and missing target are idempotent", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		if err := fs.MkDir("/parent", 0o755); err != nil {
			t.Fatalf("MkDir /parent: %v", err)
		}
		if err := fs.DeleteDir("/missing/dir"); err != nil {
			t.Fatalf("removeDir missing parent: %v", err)
		}
		if err := fs.DeleteDir("/parent/missing"); err != nil {
			t.Fatalf("removeDir missing target: %v", err)
		}
	})

	t.Run("removeFile zeroDirEntry error", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		if err := fs.WriteFile("/foo", []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		rw := ext4.CloneFSImage(t, fs)
		sb := ext4.CloneSuperblockFromFS(fs)
		rootOff := int64(ext4.RootDirBlockFor(t, rw, sb)) * int64(sb.BlockSize)
		rw.WriteHook = func(off int64, _ []byte) error {
			if off == rootOff {
				return errBoom
			}
			return nil
		}
		if err := ext4.RemoveFile(rw, 0, sb, "/foo"); err == nil {
			t.Fatalf("expected removeFile to fail when zeroing the directory entry")
		}
	})

	t.Run("zeroDirEntry extents error", func(t *testing.T) {
		if err := ext4.ZeroDirEntry(ext4.NewHookMemFile(make([]byte, 4096)), 0, &ext4.Superblock{BlockSize: 1024}, ext4.NewTestInode(ext4.RootIno, 256), "foo"); err == nil {
			t.Fatalf("expected zeroDirEntry to fail when the directory extents are invalid")
		}
	})

	t.Run("zeroDirEntry block read error", func(t *testing.T) {
		rw := ext4.NewHookMemFile(make([]byte, 4096))
		sb := &ext4.Superblock{BlockSize: 1024, InodeSize: 256}
		dir := ext4.NewTestInode(ext4.RootIno, 256)
		if err := ext4.SetInlineExtents(dir, []ext4.ExtentLeaf{{LogBlock: 0, PhysBlock: 2, Count: 1}}); err != nil {
			t.Fatalf("SetInlineExtents: %v", err)
		}
		rw.ReadHook = func(off int64, _ []byte) error {
			if off == 2*int64(sb.BlockSize) {
				return errBoom
			}
			return nil
		}
		if err := ext4.ZeroDirEntry(rw, 0, sb, dir, "foo"); err == nil {
			t.Fatalf("expected zeroDirEntry to fail when reading a directory block")
		}
	})

	t.Run("zeroDirEntry missing entry is idempotent", func(t *testing.T) {
		rw := ext4.NewHookMemFile(make([]byte, 4096))
		sb := &ext4.Superblock{BlockSize: 1024, InodeSize: 256}
		dir := ext4.NewTestInode(ext4.RootIno, 256)
		buf := make([]byte, sb.BlockSize)
		binary.LittleEndian.PutUint32(buf[0:], 2)
		binary.LittleEndian.PutUint16(buf[4:], uint16(sb.BlockSize))
		buf[6] = 3
		buf[7] = ext4.FtRegFile
		copy(buf[8:], "bar")
		if err := ext4.WriteRawBlock(rw, 0, sb, 2, buf); err != nil {
			t.Fatalf("writeRawBlock: %v", err)
		}
		if err := ext4.SetInlineExtents(dir, []ext4.ExtentLeaf{{LogBlock: 0, PhysBlock: 2, Count: 1}}); err != nil {
			t.Fatalf("SetInlineExtents: %v", err)
		}
		if err := ext4.ZeroDirEntry(rw, 0, sb, dir, "foo"); err != nil {
			t.Fatalf("zeroDirEntry missing entry: %v", err)
		}
	})

	t.Run("decrementAndFreeInode free blocks error", func(t *testing.T) {
		in := ext4.NewTestInode(7, 256)
		binary.LittleEndian.PutUint16(ext4.InodeRaw(in)[ext4.InodeOffLinksCount:], 0)
		binary.LittleEndian.PutUint32(ext4.InodeRaw(in)[ext4.InodeOffFlags:], ext4.InodeFlagExtents)
		if err := ext4.DecrementAndFreeInode(ext4.NewHookMemFile(make([]byte, 4096)), 0, &ext4.Superblock{BlockSize: 1024}, in); err == nil {
			t.Fatalf("expected decrementAndFreeInode to fail when freeing data blocks")
		}
	})

	t.Run("freeInodeSlot readBGD error", func(t *testing.T) {
		rw, sb := newFreeInodeSlotState(t)
		rw.ReadHook = func(off int64, _ []byte) error {
			if off == ext4.BgdOffset(sb, 0) {
				return errBoom
			}
			return nil
		}
		if err := ext4.FreeInodeSlot(rw, 0, sb, 5); err == nil {
			t.Fatalf("expected freeInodeSlot to fail while reading the BGD")
		}
	})

	t.Run("freeInodeSlot read bitmap error", func(t *testing.T) {
		rw, sb := newFreeInodeSlotState(t)
		rw.ReadHook = func(off int64, _ []byte) error {
			if off == 2*int64(sb.BlockSize) {
				return errBoom
			}
			return nil
		}
		if err := ext4.FreeInodeSlot(rw, 0, sb, 5); err == nil {
			t.Fatalf("expected freeInodeSlot to fail while reading the inode bitmap")
		}
	})

	t.Run("freeInodeSlot write bitmap error", func(t *testing.T) {
		rw, sb := newFreeInodeSlotState(t)
		rw.WriteHook = func(off int64, _ []byte) error {
			if off == 2*int64(sb.BlockSize) {
				return errBoom
			}
			return nil
		}
		if err := ext4.FreeInodeSlot(rw, 0, sb, 5); err == nil {
			t.Fatalf("expected freeInodeSlot to fail while writing the inode bitmap")
		}
	})

	t.Run("freeInodeSlot write BGD error", func(t *testing.T) {
		rw, sb := newFreeInodeSlotState(t)
		rw.WriteHook = func(off int64, _ []byte) error {
			if off == ext4.BgdOffset(sb, 0) {
				return errBoom
			}
			return nil
		}
		if err := ext4.FreeInodeSlot(rw, 0, sb, 5); err == nil {
			t.Fatalf("expected freeInodeSlot to fail while writing the BGD")
		}
	})

	t.Run("removeDir readDir error", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		if err := fs.MkDir("/tree", 0o755); err != nil {
			t.Fatalf("MkDir /tree: %v", err)
		}
		rw := ext4.CloneFSImage(t, fs)
		sb := ext4.CloneSuperblockFromFS(fs)
		treeBlock := ext4.DirBlockForPath(t, rw, sb, "/tree")
		rw.ReadHook = func(off int64, _ []byte) error {
			if off == int64(treeBlock)*int64(sb.BlockSize) {
				return errBoom
			}
			return nil
		}
		if err := ext4.RemoveDir(rw, 0, sb, "/tree"); err == nil {
			t.Fatalf("expected removeDir to fail while listing the target directory")
		}
	})

	t.Run("removeDir child directory error", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		if err := fs.MkDir("/tree", 0o755); err != nil {
			t.Fatalf("MkDir /tree: %v", err)
		}
		if err := fs.MkDir("/tree/sub", 0o755); err != nil {
			t.Fatalf("MkDir /tree/sub: %v", err)
		}
		rw := ext4.CloneFSImage(t, fs)
		sb := ext4.CloneSuperblockFromFS(fs)
		oldFn := ext4.SetRemoveDirChildDir(func(_ ext4.ReaderWriterAt, _ int64, _ *ext4.Superblock, _ string) error { return errBoom })
		t.Cleanup(func() { ext4.SetRemoveDirChildDir(oldFn) })
		if err := ext4.RemoveDir(rw, 0, sb, "/tree"); err == nil {
			t.Fatalf("expected removeDir to fail while recursively removing a child directory")
		}
	})

	t.Run("removeDir child file error", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		if err := fs.MkDir("/tree", 0o755); err != nil {
			t.Fatalf("MkDir /tree: %v", err)
		}
		if err := fs.WriteFile("/tree/file.txt", []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile /tree/file.txt: %v", err)
		}
		rw := ext4.CloneFSImage(t, fs)
		sb := ext4.CloneSuperblockFromFS(fs)
		oldFn := ext4.SetRemoveDirChildFile(func(_ ext4.ReaderWriterAt, _ int64, _ *ext4.Superblock, _ string) error { return errBoom })
		t.Cleanup(func() { ext4.SetRemoveDirChildFile(oldFn) })
		if err := ext4.RemoveDir(rw, 0, sb, "/tree"); err == nil {
			t.Fatalf("expected removeDir to fail while recursively removing a child file")
		}
	})

	t.Run("removeDir zeroDirEntry error", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		if err := fs.MkDir("/tree", 0o755); err != nil {
			t.Fatalf("MkDir /tree: %v", err)
		}
		rw := ext4.CloneFSImage(t, fs)
		sb := ext4.CloneSuperblockFromFS(fs)
		rootOff := int64(ext4.RootDirBlockFor(t, rw, sb)) * int64(sb.BlockSize)
		rw.WriteHook = func(off int64, _ []byte) error {
			if off == rootOff {
				return errBoom
			}
			return nil
		}
		if err := ext4.RemoveDir(rw, 0, sb, "/tree"); err == nil {
			t.Fatalf("expected removeDir to fail while zeroing the parent entry")
		}
	})

	t.Run("removeDir freeInodeBlocks error", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		if err := fs.MkDir("/tree", 0o755); err != nil {
			t.Fatalf("MkDir /tree: %v", err)
		}
		rw := ext4.CloneFSImage(t, fs)
		sb := ext4.CloneSuperblockFromFS(fs)
		oldFn := ext4.SetRemoveDirFreeBlocks(func(_ ext4.ReaderWriterAt, _ int64, _ *ext4.Superblock, _ *ext4.Inode) error { return errBoom })
		t.Cleanup(func() { ext4.SetRemoveDirFreeBlocks(oldFn) })
		if err := ext4.RemoveDir(rw, 0, sb, "/tree"); err == nil {
			t.Fatalf("expected removeDir to fail while freeing directory blocks")
		}
	})

	t.Run("removeDir freeInodeSlot error", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		if err := fs.MkDir("/tree", 0o755); err != nil {
			t.Fatalf("MkDir /tree: %v", err)
		}
		rw := ext4.CloneFSImage(t, fs)
		sb := ext4.CloneSuperblockFromFS(fs)
		oldFn := ext4.SetRemoveDirFreeSlot(func(_ ext4.ReaderWriterAt, _ int64, _ *ext4.Superblock, _ uint32) error { return errBoom })
		t.Cleanup(func() { ext4.SetRemoveDirFreeSlot(oldFn) })
		if err := ext4.RemoveDir(rw, 0, sb, "/tree"); err == nil {
			t.Fatalf("expected removeDir to fail while freeing the inode slot")
		}
	})
}
