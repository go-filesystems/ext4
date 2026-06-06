package filesystem_ext4_test

import (
	"encoding/binary"
	"errors"
	"testing"

	ext4 "github.com/go-filesystems/ext4"
)

func TestWriteFileRemainingErrorPaths(t *testing.T) {
	errBoom := errors.New("boom")

	t.Run("writeFile parent readDir error", func(t *testing.T) {
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
		if err := ext4.WriteFileRaw(rw, 0, sb, "/foo", []byte("x"), 0o644); err == nil {
			t.Fatalf("expected writeFile to fail when reading parent directory")
		}
	})

	t.Run("writeFile existing inode read error", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		if err := fs.WriteFile("/foo", []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		rw := ext4.CloneFSImage(t, fs)
		sb := ext4.CloneSuperblockFromFS(fs)
		st, err := fs.Stat("/foo")
		if err != nil {
			t.Fatalf("Stat /foo: %v", err)
		}
		childOff := ext4.InodeOffsetFor(rw, sb, uint32(st.Inode()))
		rw.ReadHook = func(off int64, _ []byte) error {
			if off == childOff {
				return errBoom
			}
			return nil
		}
		if err := ext4.WriteFileRaw(rw, 0, sb, "/foo", []byte("y"), 0o644); err == nil {
			t.Fatalf("expected writeFile to fail when reading existing inode")
		}
	})

	t.Run("writeFile existing inode free error", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		if err := fs.WriteFile("/foo", []byte("payload"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		rw := ext4.CloneFSImage(t, fs)
		sb := ext4.CloneSuperblockFromFS(fs)
		in, err := ext4.LookupPath(rw, 0, sb, "/foo")
		if err != nil {
			t.Fatalf("lookupPath /foo: %v", err)
		}
		raw := ext4.InodeRaw(in)
		for i := range raw[ext4.InodeOffBlock : ext4.InodeOffBlock+60] {
			raw[ext4.InodeOffBlock+i] = 0
		}
		binary.LittleEndian.PutUint32(raw[ext4.InodeOffFlags:], ext4.InodeFlagExtents)
		if err := ext4.WriteInode(rw, 0, sb, in); err != nil {
			t.Fatalf("writeInode: %v", err)
		}
		if err := ext4.WriteFileRaw(rw, 0, sb, "/foo", []byte("new"), 0o644); err == nil {
			t.Fatalf("expected writeFile to fail when freeing old blocks")
		}
	})

	t.Run("writeFile allocates a block for empty data", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		rw := ext4.CloneFSImage(t, fs)
		sb := ext4.CloneSuperblockFromFS(fs)
		if err := ext4.WriteFileRaw(rw, 0, sb, "/foo", []byte(""), 0o644); err != nil {
			t.Fatalf("writeFile allocate block: %v", err)
		}
	})
}
