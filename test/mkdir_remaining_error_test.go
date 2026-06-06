package filesystem_ext4_test

import (
	"encoding/binary"
	"errors"
	"testing"

	ext4 "github.com/go-filesystems/ext4"
)

func TestMakeDirRemainingErrorPaths(t *testing.T) {
	errBoom := errors.New("boom")

	t.Run("parent readDir error", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		rw := ext4.CloneFSImage(t, fs)
		sb := ext4.CloneSuperblockFromFS(fs)
		rootBlock := ext4.RootDirBlockFor(t, rw, sb)
		rw.ReadHook = func(off int64, _ []byte) error {
			if off == int64(rootBlock)*int64(sb.BlockSize) {
				return errBoom
			}
			return nil
		}
		if err := ext4.MakeDir(rw, 0, sb, "/foo", 0o755); err == nil {
			t.Fatalf("expected makeDir to fail while reading the parent directory")
		}
	})

	t.Run("allocInode error", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		rw := ext4.CloneFSImage(t, fs)
		sb := ext4.CloneSuperblockFromFS(fs)
		old := ext4.SetMakeDirAllocInode(func(r ext4.ReaderWriterAt, off int64, s *ext4.Superblock, isDir bool) (uint32, error) {
			return 0, errBoom
		})
		t.Cleanup(func() { ext4.SetMakeDirAllocInode(old) })
		if err := ext4.MakeDir(rw, 0, sb, "/foo", 0o755); !errors.Is(err, errBoom) {
			t.Fatalf("expected makeDir allocInode error %v, got %v", errBoom, err)
		}
	})

	t.Run("allocBlocks error", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		rw := ext4.CloneFSImage(t, fs)
		sb := ext4.CloneSuperblockFromFS(fs)
		old := ext4.SetMakeDirAllocBlocks(func(r ext4.ReaderWriterAt, off int64, s *ext4.Superblock, n uint32) ([]uint64, error) {
			return nil, errBoom
		})
		t.Cleanup(func() { ext4.SetMakeDirAllocBlocks(old) })
		if err := ext4.MakeDir(rw, 0, sb, "/foo", 0o755); !errors.Is(err, errBoom) {
			t.Fatalf("expected makeDir allocBlocks error %v, got %v", errBoom, err)
		}
	})

	t.Run("metadata checksum tail", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		sb := ext4.CloneSuperblockFromFS(fs)
		sb.FeatureROCompat |= ext4.FeatROCompatMetadataCsum
		rw := ext4.CloneFSImage(t, fs)
		if err := ext4.MakeDir(rw, 0, sb, "/foo", 0o755); err != nil {
			t.Fatalf("makeDir: %v", err)
		}
		_, err := ext4.LookupPath(rw, 0, sb, "/foo")
		if err != nil {
			t.Fatalf("lookupPath(/foo): %v", err)
		}
		// Read first data block of the new directory and check tail
		blk := ext4.DirBlockForPath(t, rw, sb, "/foo")
		buf, err := ext4.ReadRawBlock(rw, 0, sb, blk)
		if err != nil {
			t.Fatalf("readRawBlock: %v", err)
		}
		tailOff := len(buf) - 12
		if binary.LittleEndian.Uint16(buf[tailOff+4:]) != 12 {
			t.Fatalf("tail rec_len = %d, want 12", binary.LittleEndian.Uint16(buf[tailOff+4:]))
		}
		if buf[tailOff+7] != ext4.FtDirTail {
			t.Fatalf("tail file_type = %d, want %d", buf[tailOff+7], ext4.FtDirTail)
		}
	})

	t.Run("directory block write error", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		rw := ext4.CloneFSImage(t, fs)
		sb := ext4.CloneSuperblockFromFS(fs)
		d, err := ext4.ReadBGD(rw, 0, sb, 0)
		if err != nil {
			t.Fatalf("readBGD: %v", err)
		}
		allowedWrites := map[int64]bool{
			1024:                  true,
			ext4.BgdOffset(sb, 0): true,
			int64(d.BlockBitmapBlock) * int64(sb.BlockSize): true,
			int64(d.InodeBitmapBlock) * int64(sb.BlockSize): true,
		}
		rw.WriteHook = func(off int64, _ []byte) error {
			if !allowedWrites[off] {
				return errBoom
			}
			return nil
		}
		if err := ext4.MakeDir(rw, 0, sb, "/foo", 0o755); err == nil {
			t.Fatalf("expected makeDir to fail when writing the new directory block")
		}
	})

	t.Run("new inode write error", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		rw := ext4.CloneFSImage(t, fs)
		sb := ext4.CloneSuperblockFromFS(fs)
		newInodeOff := ext4.InodeOffsetFor(rw, sb, 11)
		rw.WriteHook = func(off int64, _ []byte) error {
			if off == newInodeOff {
				return errBoom
			}
			return nil
		}
		if err := ext4.MakeDir(rw, 0, sb, "/foo", 0o755); err == nil {
			t.Fatalf("expected makeDir to fail when writing the new inode")
		}
	})

	t.Run("parent addDirEntry error", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		rw := ext4.CloneFSImage(t, fs)
		sb := ext4.CloneSuperblockFromFS(fs)
		rootBlock := ext4.RootDirBlockFor(t, rw, sb)
		rootReads := 0
		rw.ReadHook = func(off int64, _ []byte) error {
			if off == int64(rootBlock)*int64(sb.BlockSize) {
				rootReads++
				if rootReads == 2 {
					return errBoom
				}
			}
			return nil
		}
		if err := ext4.MakeDir(rw, 0, sb, "/foo", 0o755); err == nil {
			t.Fatalf("expected makeDir to fail while adding the entry to the parent")
		}
	})
}
