package filesystem_ext4_test

import (
	"encoding/binary"
	"errors"
	"testing"

	ext4 "github.com/go-filesystems/ext4"
)

func TestRenameRemainingErrorPaths(t *testing.T) {
	errBoom := errors.New("boom")

	t.Run("source parent error", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		rw := ext4.CloneFSImage(t, fs)
		sb := ext4.CloneSuperblockFromFS(fs)
		if err := ext4.Rename(rw, 0, sb, "/missing/file.txt", "/dst.txt"); err == nil {
			t.Fatalf("expected rename to fail when the source parent is missing")
		}
	})

	t.Run("source readDir error", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		if err := fs.WriteFile("/src.txt", []byte("src"), 0o644); err != nil {
			t.Fatalf("WriteFile /src.txt: %v", err)
		}
		rw := ext4.CloneFSImage(t, fs)
		sb := ext4.CloneSuperblockFromFS(fs)
		rootBlock := ext4.RootDirBlockFor(t, rw, sb)
		rw.ReadHook = func(off int64, _ []byte) error {
			if off == int64(rootBlock)*int64(sb.BlockSize) {
				return errBoom
			}
			return nil
		}
		if err := ext4.Rename(rw, 0, sb, "/src.txt", "/dst.txt"); err == nil {
			t.Fatalf("expected rename to fail while reading the source parent entries")
		}
	})

	t.Run("source inode read error", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		if err := fs.WriteFile("/src.txt", []byte("src"), 0o644); err != nil {
			t.Fatalf("WriteFile /src.txt: %v", err)
		}
		rw := ext4.CloneFSImage(t, fs)
		sb := ext4.CloneSuperblockFromFS(fs)
		st, err := fs.Stat("/src.txt")
		if err != nil {
			t.Fatalf("Stat /src.txt: %v", err)
		}
		srcInodeOff := ext4.InodeOffsetFor(rw, sb, uint32(st.Inode()))
		rw.ReadHook = func(off int64, _ []byte) error {
			if off == srcInodeOff {
				return errBoom
			}
			return nil
		}
		if err := ext4.Rename(rw, 0, sb, "/src.txt", "/dst.txt"); err == nil {
			t.Fatalf("expected rename to fail while reading the source inode")
		}
	})

	t.Run("destination readDir error", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		if err := fs.WriteFile("/src.txt", []byte("src"), 0o644); err != nil {
			t.Fatalf("WriteFile /src.txt: %v", err)
		}
		if err := fs.MkDir("/dst", 0o755); err != nil {
			t.Fatalf("MkDir /dst: %v", err)
		}
		rw := ext4.CloneFSImage(t, fs)
		sb := ext4.CloneSuperblockFromFS(fs)
		dstBlock := ext4.DirBlockForPath(t, rw, sb, "/dst")
		rw.ReadHook = func(off int64, _ []byte) error {
			if off == int64(dstBlock)*int64(sb.BlockSize) {
				return errBoom
			}
			return nil
		}
		if err := ext4.Rename(rw, 0, sb, "/src.txt", "/dst/src.txt"); err == nil {
			t.Fatalf("expected rename to fail while reading the destination parent")
		}
	})

	t.Run("destination inode read error", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		if err := fs.WriteFile("/src.txt", []byte("src"), 0o644); err != nil {
			t.Fatalf("WriteFile /src.txt: %v", err)
		}
		if err := fs.WriteFile("/dst.txt", []byte("dst"), 0o644); err != nil {
			t.Fatalf("WriteFile /dst.txt: %v", err)
		}
		rw := ext4.CloneFSImage(t, fs)
		sb := ext4.CloneSuperblockFromFS(fs)
		st, err := fs.Stat("/dst.txt")
		if err != nil {
			t.Fatalf("Stat /dst.txt: %v", err)
		}
		dstInodeOff := ext4.InodeOffsetFor(rw, sb, uint32(st.Inode()))
		rw.ReadHook = func(off int64, _ []byte) error {
			if off == dstInodeOff {
				return errBoom
			}
			return nil
		}
		if err := ext4.Rename(rw, 0, sb, "/src.txt", "/dst.txt"); err == nil {
			t.Fatalf("expected rename to fail while reading the destination inode")
		}
	})

	t.Run("removeDir error", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		if err := fs.MkDir("/srcdir", 0o755); err != nil {
			t.Fatalf("MkDir /srcdir: %v", err)
		}
		if err := fs.MkDir("/destdir", 0o755); err != nil {
			t.Fatalf("MkDir /destdir: %v", err)
		}
		rw := ext4.CloneFSImage(t, fs)
		sb := ext4.CloneSuperblockFromFS(fs)
		// corrupt inode extents for /destdir
		in, err := ext4.LookupPath(rw, 0, sb, "/destdir")
		if err != nil {
			t.Fatalf("lookupPath: %v", err)
		}
		raw := ext4.InodeRaw(in)
		for i := range raw[ext4.InodeOffBlock : ext4.InodeOffBlock+60] {
			raw[ext4.InodeOffBlock+i] = 0
		}
		binary.LittleEndian.PutUint32(raw[ext4.InodeOffFlags:], ext4.InodeFlagExtents)
		if err := ext4.WriteInode(rw, 0, sb, in); err != nil {
			t.Fatalf("writeInode: %v", err)
		}
		if err := ext4.Rename(rw, 0, sb, "/srcdir", "/destdir"); err == nil {
			t.Fatalf("expected rename to fail while removing the destination directory")
		}
	})

	t.Run("removeFile error", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		if err := fs.WriteFile("/src.txt", []byte("src"), 0o644); err != nil {
			t.Fatalf("WriteFile /src.txt: %v", err)
		}
		if err := fs.WriteFile("/dest.txt", []byte("dst"), 0o644); err != nil {
			t.Fatalf("WriteFile /dest.txt: %v", err)
		}
		rw := ext4.CloneFSImage(t, fs)
		sb := ext4.CloneSuperblockFromFS(fs)
		in, err := ext4.LookupPath(rw, 0, sb, "/dest.txt")
		if err != nil {
			t.Fatalf("lookupPath: %v", err)
		}
		raw := ext4.InodeRaw(in)
		for i := range raw[ext4.InodeOffBlock : ext4.InodeOffBlock+60] {
			raw[ext4.InodeOffBlock+i] = 0
		}
		binary.LittleEndian.PutUint32(raw[ext4.InodeOffFlags:], ext4.InodeFlagExtents)
		if err := ext4.WriteInode(rw, 0, sb, in); err != nil {
			t.Fatalf("writeInode: %v", err)
		}
		if err := ext4.Rename(rw, 0, sb, "/src.txt", "/dest.txt"); err == nil {
			t.Fatalf("expected rename to fail while removing the destination file")
		}
	})

	t.Run("symlink file type", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		if err := fs.WriteFile("/target", []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile /target: %v", err)
		}
		ext4.AddFastSymlink(t, fs, "/", "ln", "/target")
		rw := ext4.CloneFSImage(t, fs)
		sb := ext4.CloneSuperblockFromFS(fs)
		if err := ext4.Rename(rw, 0, sb, "/ln", "/moved"); err != nil {
			t.Fatalf("rename symlink: %v", err)
		}
		root, err := ext4.ReadInode(rw, 0, sb, ext4.RootIno)
		if err != nil {
			t.Fatalf("readInode(root): %v", err)
		}
		entries, err := ext4.ReadDir(rw, 0, sb, root)
		if err != nil {
			t.Fatalf("readDir(root): %v", err)
		}
		for _, entry := range entries {
			if entry.Name == "moved" {
				if entry.FileType != ext4.FtSymlink {
					t.Fatalf("entry file type = %d, want %d", entry.FileType, ext4.FtSymlink)
				}
				return
			}
		}
		t.Fatalf("moved symlink entry not found")
	})

	t.Run("addDirEntry error", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		if err := fs.WriteFile("/src.txt", []byte("src"), 0o644); err != nil {
			t.Fatalf("WriteFile /src.txt: %v", err)
		}
		if err := fs.MkDir("/dst", 0o755); err != nil {
			t.Fatalf("MkDir /dst: %v", err)
		}
		rw := ext4.CloneFSImage(t, fs)
		sb := ext4.CloneSuperblockFromFS(fs)
		dstOff := int64(ext4.DirBlockForPath(t, rw, sb, "/dst")) * int64(sb.BlockSize)
		dstReads := 0
		rw.ReadHook = func(off int64, _ []byte) error {
			if off == dstOff {
				dstReads++
				if dstReads == 2 {
					return errBoom
				}
			}
			return nil
		}
		if err := ext4.Rename(rw, 0, sb, "/src.txt", "/dst/src.txt"); err == nil {
			t.Fatalf("expected rename to fail while adding the destination entry")
		}
	})

	t.Run("zeroDirEntry error", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		if err := fs.MkDir("/src", 0o755); err != nil {
			t.Fatalf("MkDir /src: %v", err)
		}
		if err := fs.MkDir("/dst", 0o755); err != nil {
			t.Fatalf("MkDir /dst: %v", err)
		}
		if err := fs.WriteFile("/src/file.txt", []byte("src"), 0o644); err != nil {
			t.Fatalf("WriteFile /src/file.txt: %v", err)
		}
		rw := ext4.CloneFSImage(t, fs)
		sb := ext4.CloneSuperblockFromFS(fs)
		srcOff := int64(ext4.DirBlockForPath(t, rw, sb, "/src")) * int64(sb.BlockSize)
		srcReads := 0
		rw.ReadHook = func(off int64, _ []byte) error {
			if off == srcOff {
				srcReads++
				if srcReads == 2 {
					return errBoom
				}
			}
			return nil
		}
		if err := ext4.Rename(rw, 0, sb, "/src/file.txt", "/dst/file.txt"); err == nil {
			t.Fatalf("expected rename to fail while removing the source entry")
		}
	})

	t.Run("updateDotDot error", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		if err := fs.MkDir("/src", 0o755); err != nil {
			t.Fatalf("MkDir /src: %v", err)
		}
		if err := fs.MkDir("/dst", 0o755); err != nil {
			t.Fatalf("MkDir /dst: %v", err)
		}
		if err := fs.MkDir("/src/sub", 0o755); err != nil {
			t.Fatalf("MkDir /src/sub: %v", err)
		}
		rw := ext4.CloneFSImage(t, fs)
		sb := ext4.CloneSuperblockFromFS(fs)
		subOff := int64(ext4.DirBlockForPath(t, rw, sb, "/src/sub")) * int64(sb.BlockSize)
		rw.ReadHook = func(off int64, _ []byte) error {
			if off == subOff {
				return errBoom
			}
			return nil
		}
		if err := ext4.Rename(rw, 0, sb, "/src/sub", "/dst/sub"); err == nil {
			t.Fatalf("expected rename to fail while updating '..'")
		}
	})

	t.Run("write old parent error", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		if err := fs.MkDir("/src", 0o755); err != nil {
			t.Fatalf("MkDir /src: %v", err)
		}
		if err := fs.MkDir("/dst", 0o755); err != nil {
			t.Fatalf("MkDir /dst: %v", err)
		}
		if err := fs.MkDir("/src/sub", 0o755); err != nil {
			t.Fatalf("MkDir /src/sub: %v", err)
		}
		rw := ext4.CloneFSImage(t, fs)
		sb := ext4.CloneSuperblockFromFS(fs)
		// Determine the inode offset for the old parent (/src).
		oldParentIno, err := ext4.LookupPath(rw, 0, sb, "/src")
		if err != nil {
			t.Fatalf("lookupPath /src: %v", err)
		}
		oldParentOff := ext4.InodeOffsetFor(rw, sb, ext4.InodeNum(oldParentIno))
		rw.WriteHook = func(off int64, p []byte) error {
			if oldParentOff >= off && oldParentOff < off+int64(len(p)) {
				return errBoom
			}
			return nil
		}
		if err := ext4.Rename(rw, 0, sb, "/src/sub", "/dst/sub"); err == nil {
			t.Fatalf("expected rename to fail while writing the old parent inode")
		}
	})

	t.Run("write new parent error", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		if err := fs.MkDir("/src", 0o755); err != nil {
			t.Fatalf("MkDir /src: %v", err)
		}
		if err := fs.MkDir("/dst", 0o755); err != nil {
			t.Fatalf("MkDir /dst: %v", err)
		}
		if err := fs.MkDir("/src/sub", 0o755); err != nil {
			t.Fatalf("MkDir /src/sub: %v", err)
		}
		rw := ext4.CloneFSImage(t, fs)
		sb := ext4.CloneSuperblockFromFS(fs)
		// Determine the inode offset for the new parent (/dst).
		newParentIno, err := ext4.LookupPath(rw, 0, sb, "/dst")
		if err != nil {
			t.Fatalf("lookupPath /dst: %v", err)
		}
		newParentOff := ext4.InodeOffsetFor(rw, sb, ext4.InodeNum(newParentIno))
		rw.WriteHook = func(off int64, p []byte) error {
			if newParentOff >= off && newParentOff < off+int64(len(p)) {
				return errBoom
			}
			return nil
		}
		if err := ext4.Rename(rw, 0, sb, "/src/sub", "/dst/sub"); err == nil {
			t.Fatalf("expected rename to fail while writing the new parent inode")
		}
	})

	t.Run("updateDotDot extents error", func(t *testing.T) {
		if err := ext4.UpdateDotDot(ext4.NewHookMemFile(make([]byte, 4096)), 0, &ext4.Superblock{BlockSize: 1024}, ext4.NewTestInode(1, 256), 2); err == nil {
			t.Fatalf("expected updateDotDot to fail on invalid extents")
		}
	})

	t.Run("updateDotDot block read error", func(t *testing.T) {
		rw := ext4.NewHookMemFile(make([]byte, 4096))
		sb := &ext4.Superblock{BlockSize: 1024, InodeSize: 256}
		dir := ext4.NewTestInode(2, 256)
		if err := ext4.SetInlineExtents(dir, []ext4.ExtentLeaf{{LogBlock: 0, PhysBlock: 2, Count: 1}}); err != nil {
			t.Fatalf("SetInlineExtents: %v", err)
		}
		rw.ReadHook = func(off int64, _ []byte) error {
			if off == 2*int64(sb.BlockSize) {
				return errBoom
			}
			return nil
		}
		if err := ext4.UpdateDotDot(rw, 0, sb, dir, 3); err == nil {
			t.Fatalf("expected updateDotDot to fail while reading the directory block")
		}
	})

	t.Run("updateDotDot stops on short record", func(t *testing.T) {
		rw := ext4.NewHookMemFile(make([]byte, 4096))
		sb := &ext4.Superblock{BlockSize: 1024, InodeSize: 256}
		dir := ext4.NewTestInode(2, 256)
		if err := ext4.SetInlineExtents(dir, []ext4.ExtentLeaf{{LogBlock: 0, PhysBlock: 2, Count: 1}}); err != nil {
			t.Fatalf("SetInlineExtents: %v", err)
		}
		buf := make([]byte, sb.BlockSize)
		binary.LittleEndian.PutUint16(buf[4:], 4)
		if err := ext4.WriteRawBlock(rw, 0, sb, 2, buf); err != nil {
			t.Fatalf("writeRawBlock: %v", err)
		}
		if err := ext4.UpdateDotDot(rw, 0, sb, dir, 3); err != nil {
			t.Fatalf("updateDotDot short record: %v", err)
		}
	})

	t.Run("updateDotDot stops on checksum tail", func(t *testing.T) {
		rw := ext4.NewHookMemFile(make([]byte, 4096))
		sb := &ext4.Superblock{BlockSize: 1024, InodeSize: 256}
		dir := ext4.NewTestInode(2, 256)
		if err := ext4.SetInlineExtents(dir, []ext4.ExtentLeaf{{LogBlock: 0, PhysBlock: 2, Count: 1}}); err != nil {
			t.Fatalf("SetInlineExtents: %v", err)
		}
		buf := make([]byte, sb.BlockSize)
		binary.LittleEndian.PutUint16(buf[4:], 12)
		buf[7] = ext4.FtDirTail
		if err := ext4.WriteRawBlock(rw, 0, sb, 2, buf); err != nil {
			t.Fatalf("writeRawBlock: %v", err)
		}
		if err := ext4.UpdateDotDot(rw, 0, sb, dir, 3); err != nil {
			t.Fatalf("updateDotDot checksum tail: %v", err)
		}
	})
}
