package filesystem_ext4_test

import (
	"errors"
	"testing"

	ext4 "github.com/go-filesystems/ext4"
)

func TestAllocErrorPaths(t *testing.T) {
	errBoom := errors.New("boom")
	fs, cleanup := ext4.NewTempFS(t)
	defer cleanup()

	newState := func(t *testing.T) (*ext4.HookMemFile, *ext4.Superblock, *ext4.Bgd, int64, int64, int64) {
		t.Helper()
		rw := ext4.CloneFSImage(t, fs)
		sb := ext4.CloneSuperblockFromFS(fs)
		d, err := ext4.ReadBGD(rw, 0, sb, 0)
		if err != nil {
			t.Fatalf("readBGD: %v", err)
		}
		return rw, sb, d,
			int64(d.InodeBitmapBlock) * int64(sb.BlockSize),
			int64(d.BlockBitmapBlock) * int64(sb.BlockSize),
			ext4.BgdOffset(sb, 0)
	}

	t.Run("writeBitmapWithCsum read error", func(t *testing.T) {
		rw, sb, d, inodeBitmapOff, _, _ := newState(t)
		rw.ReadHook = func(off int64, _ []byte) error {
			if off == inodeBitmapOff {
				return errBoom
			}
			return nil
		}
		if err := ext4.WriteBitmapWithCsum(rw, 0, sb, 0, d, false); err == nil {
			t.Fatalf("expected writeBitmapWithCsum to fail on bitmap read")
		}
	})

	t.Run("allocInode readBGD error", func(t *testing.T) {
		rw, sb, _, _, _, bgdOff := newState(t)
		rw.ReadHook = func(off int64, _ []byte) error {
			if off == bgdOff {
				return errBoom
			}
			return nil
		}
		if _, err := ext4.AllocInode(rw, 0, sb, false); err == nil {
			t.Fatalf("expected allocInode to fail on BGD read")
		}
	})

	t.Run("allocInode readBitmap error", func(t *testing.T) {
		rw, sb, _, inodeBitmapOff, _, _ := newState(t)
		rw.ReadHook = func(off int64, _ []byte) error {
			if off == inodeBitmapOff {
				return errBoom
			}
			return nil
		}
		if _, err := ext4.AllocInode(rw, 0, sb, false); err == nil {
			t.Fatalf("expected allocInode to fail on inode bitmap read")
		}
	})

	t.Run("allocInode continues past full bitmap", func(t *testing.T) {
		rw, sb, d, _, _, _ := newState(t)
		d.FreeInodesCount = 1
		if err := ext4.WriteBGD(rw, 0, sb, 0, d); err != nil {
			t.Fatalf("writeBGD: %v", err)
		}
		bmap := make([]byte, sb.BlockSize)
		for i := range bmap {
			bmap[i] = 0xFF
		}
		if err := ext4.WriteRawBlock(rw, 0, sb, d.InodeBitmapBlock, bmap); err != nil {
			t.Fatalf("WriteRawBlock: %v", err)
		}
		if _, err := ext4.AllocInode(rw, 0, sb, false); err == nil {
			t.Fatalf("expected allocInode to fail when no inode bit is free")
		}
	})

	t.Run("allocInode bitmap write error", func(t *testing.T) {
		rw, sb, _, inodeBitmapOff, _, _ := newState(t)
		rw.WriteHook = func(off int64, _ []byte) error {
			if off == inodeBitmapOff {
				return errBoom
			}
			return nil
		}
		if _, err := ext4.AllocInode(rw, 0, sb, false); err == nil {
			t.Fatalf("expected allocInode to fail on inode bitmap write")
		}
	})

	t.Run("allocInode bgd write error", func(t *testing.T) {
		rw, sb, _, _, _, bgdOff := newState(t)
		rw.WriteHook = func(off int64, _ []byte) error {
			if off == bgdOff {
				return errBoom
			}
			return nil
		}
		if _, err := ext4.AllocInode(rw, 0, sb, false); err == nil {
			t.Fatalf("expected allocInode to fail on BGD write")
		}
	})

	t.Run("allocInode superblock write error", func(t *testing.T) {
		rw, sb, _, _, _, _ := newState(t)
		rw.WriteHook = func(off int64, _ []byte) error {
			if off == 1024 {
				return errBoom
			}
			return nil
		}
		if _, err := ext4.AllocInode(rw, 0, sb, false); err == nil {
			t.Fatalf("expected allocInode to fail on superblock write")
		}
	})

	t.Run("allocBlocks readBGD error", func(t *testing.T) {
		rw, sb, _, _, _, bgdOff := newState(t)
		rw.ReadHook = func(off int64, _ []byte) error {
			if off == bgdOff {
				return errBoom
			}
			return nil
		}
		if _, err := ext4.AllocBlocks(rw, 0, sb, 1); err == nil {
			t.Fatalf("expected allocBlocks to fail on BGD read")
		}
	})

	t.Run("allocBlocks readBitmap error", func(t *testing.T) {
		rw, sb, _, _, blockBitmapOff, _ := newState(t)
		rw.ReadHook = func(off int64, _ []byte) error {
			if off == blockBitmapOff {
				return errBoom
			}
			return nil
		}
		if _, err := ext4.AllocBlocks(rw, 0, sb, 1); err == nil {
			t.Fatalf("expected allocBlocks to fail on block bitmap read")
		}
	})

	t.Run("allocBlocks continues past full bitmap", func(t *testing.T) {
		rw, sb, d, _, _, _ := newState(t)
		d.FreeBlocksCount = 1
		if err := ext4.WriteBGD(rw, 0, sb, 0, d); err != nil {
			t.Fatalf("writeBGD: %v", err)
		}
		bmap := make([]byte, sb.BlockSize)
		for i := range bmap {
			bmap[i] = 0xFF
		}
		if err := ext4.WriteRawBlock(rw, 0, sb, d.BlockBitmapBlock, bmap); err != nil {
			t.Fatalf("WriteRawBlock: %v", err)
		}
		if _, err := ext4.AllocBlocks(rw, 0, sb, 1); err == nil {
			t.Fatalf("expected allocBlocks to fail when no block bit is free")
		}
	})

	t.Run("allocBlocks bitmap write error", func(t *testing.T) {
		rw, sb, _, _, blockBitmapOff, _ := newState(t)
		rw.WriteHook = func(off int64, _ []byte) error {
			if off == blockBitmapOff {
				return errBoom
			}
			return nil
		}
		if _, err := ext4.AllocBlocks(rw, 0, sb, 1); err == nil {
			t.Fatalf("expected allocBlocks to fail on block bitmap write")
		}
	})

	t.Run("allocBlocks bgd write error", func(t *testing.T) {
		rw, sb, _, _, _, bgdOff := newState(t)
		rw.WriteHook = func(off int64, _ []byte) error {
			if off == bgdOff {
				return errBoom
			}
			return nil
		}
		if _, err := ext4.AllocBlocks(rw, 0, sb, 1); err == nil {
			t.Fatalf("expected allocBlocks to fail on BGD write")
		}
	})

	t.Run("allocBlocks superblock write error", func(t *testing.T) {
		rw, sb, _, _, _, _ := newState(t)
		rw.WriteHook = func(off int64, _ []byte) error {
			if off == 1024 {
				return errBoom
			}
			return nil
		}
		if _, err := ext4.AllocBlocks(rw, 0, sb, 1); err == nil {
			t.Fatalf("expected allocBlocks to fail on superblock write")
		}
	})

	t.Run("freeBlock readBGD error", func(t *testing.T) {
		rw, sb, _, _, _, bgdOff := newState(t)
		rw.ReadHook = func(off int64, _ []byte) error {
			if off == bgdOff {
				return errBoom
			}
			return nil
		}
		if err := ext4.FreeBlock(rw, 0, sb, uint64(sb.FirstDataBlock)+1); err == nil {
			t.Fatalf("expected freeBlock to fail on BGD read")
		}
	})

	t.Run("freeBlock readBitmap error", func(t *testing.T) {
		rw, sb, _, _, blockBitmapOff, _ := newState(t)
		rw.ReadHook = func(off int64, _ []byte) error {
			if off == blockBitmapOff {
				return errBoom
			}
			return nil
		}
		if err := ext4.FreeBlock(rw, 0, sb, uint64(sb.FirstDataBlock)+1); err == nil {
			t.Fatalf("expected freeBlock to fail on bitmap read")
		}
	})

	t.Run("freeBlock bitmap write error", func(t *testing.T) {
		rw, sb, _, _, blockBitmapOff, _ := newState(t)
		rw.WriteHook = func(off int64, _ []byte) error {
			if off == blockBitmapOff {
				return errBoom
			}
			return nil
		}
		if err := ext4.FreeBlock(rw, 0, sb, uint64(sb.FirstDataBlock)+1); err == nil {
			t.Fatalf("expected freeBlock to fail on bitmap write")
		}
	})

	t.Run("freeBlock bgd write error", func(t *testing.T) {
		rw, sb, _, _, _, bgdOff := newState(t)
		rw.WriteHook = func(off int64, _ []byte) error {
			if off == bgdOff {
				return errBoom
			}
			return nil
		}
		if err := ext4.FreeBlock(rw, 0, sb, uint64(sb.FirstDataBlock)+1); err == nil {
			t.Fatalf("expected freeBlock to fail on BGD write")
		}
	})
}
