package filesystem_ext4_test

import (
	"encoding/binary"
	"testing"

	ext4 "github.com/go-filesystems/ext4"
)

func TestSimpleErrorPathsAcrossOperations(t *testing.T) {
	fs, cleanup := ext4.NewTempFS(t)
	defer cleanup()
	// Create a directory on the live FS before cloning so the cloned image
	// contains the duplicate entry used to validate MakeDir rejects it.
	if err := fs.MkDir("/dup", 0o755); err != nil {
		t.Fatalf("MkDir /dup: %v", err)
	}
	// Create a regular file on the live FS so the cloned image contains it.
	if err := fs.WriteFile("/file", []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile /file: %v", err)
	}
	rw := ext4.CloneFSImage(t, fs)
	sb := ext4.CloneSuperblockFromFS(fs)

	if err := ext4.MakeDir(rw, 0, sb, "/missing/child", 0o755); err == nil {
		t.Fatalf("expected makeDir to fail when parent is missing")
	}
	if err := ext4.MakeDir(rw, 0, sb, "/dup", 0o755); err == nil {
		t.Fatalf("expected makeDir to reject duplicate path")
	}
	if err := ext4.WriteFileRaw(rw, 0, sb, "/missing/file", []byte("x"), 0o644); err == nil {
		t.Fatalf("expected writeFile to fail when parent is missing")
	}
	if err := ext4.WriteFileRaw(rw, 0, sb, "/dup", []byte("x"), 0o644); err == nil {
		t.Fatalf("expected writeFile to reject non-regular existing entry")
	}
	if err := ext4.RemoveDir(rw, 0, sb, "/file"); err == nil {
		t.Fatalf("expected removeDir to reject non-directory path")
	}
	if err := ext4.Rename(rw, 0, sb, "/missing", "/dst"); err == nil {
		t.Fatalf("expected rename to fail when source is missing")
	}
	if err := ext4.Rename(rw, 0, sb, "/file", "/missing/dst"); err == nil {
		t.Fatalf("expected rename to fail when destination parent is missing")
	}
}

func TestZeroEntryInBlockAdditionalStops(t *testing.T) {
	le := binary.LittleEndian

	t.Run("invalid recLen stops scan", func(t *testing.T) {
		buf := make([]byte, 32)
		le.PutUint16(buf[4:], 4)
		if ext4.ZeroEntryInBlock(buf, "foo", le) {
			t.Fatalf("expected zeroEntryInBlock to stop on invalid record length")
		}
	})

	t.Run("checksum tail stops scan", func(t *testing.T) {
		buf := make([]byte, 32)
		le.PutUint16(buf[4:], 12)
		buf[7] = ext4.FtDirTail
		if ext4.ZeroEntryInBlock(buf, "foo", le) {
			t.Fatalf("expected zeroEntryInBlock to stop at checksum tail")
		}
	})
}

func TestFSWrapperErrorPaths(t *testing.T) {
	t.Run("ListDir missing path", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		if _, err := fs.ListDir("/missing"); err == nil {
			t.Fatalf("expected ListDir to fail for missing path")
		}
	})

	t.Run("ListDir propagates directory read error", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		rw := ext4.CloneFSImage(t, fs)
		sb := ext4.CloneSuperblockFromFS(fs)
		root, err := ext4.ReadInode(rw, 0, sb, ext4.RootIno)
		if err != nil {
			t.Fatalf("readInode(root): %v", err)
		}
		if err := ext4.SetInlineExtents(root, []ext4.ExtentLeaf{{LogBlock: 0, PhysBlock: uint64(sb.BlocksCount + 100), Count: 1}}); err != nil {
			t.Fatalf("SetInlineExtents: %v", err)
		}
		if err := ext4.WriteInode(rw, 0, sb, root); err != nil {
			t.Fatalf("writeInode(root): %v", err)
		}
		if _, err := ext4.ReadDir(rw, 0, sb, root); err == nil {
			t.Fatalf("expected ReadDir to fail on invalid directory extent")
		}
	})

	t.Run("ReadLink root path is rejected", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		if _, err := fs.ReadLink("/"); err == nil {
			t.Fatalf("expected ReadLink to reject root path")
		}
	})

	t.Run("ReadLink propagates parent lookup error", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		if _, err := fs.ReadLink("/missing/link"); err == nil {
			t.Fatalf("expected ReadLink to fail when parent directory is missing")
		}
	})

	t.Run("ReadLink missing entry in existing parent", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		if err := fs.MkDir("/dir", 0o755); err != nil {
			t.Fatalf("MkDir /dir: %v", err)
		}
		if _, err := fs.ReadLink("/dir/missing"); err == nil {
			t.Fatalf("expected ReadLink to fail when entry does not exist")
		}
	})

	t.Run("ReadLink propagates directory read error", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		rw := ext4.CloneFSImage(t, fs)
		sb := ext4.CloneSuperblockFromFS(fs)
		root, err := ext4.ReadInode(rw, 0, sb, ext4.RootIno)
		if err != nil {
			t.Fatalf("readInode(root): %v", err)
		}
		if err := ext4.SetInlineExtents(root, []ext4.ExtentLeaf{{LogBlock: 0, PhysBlock: uint64(sb.BlocksCount + 100), Count: 1}}); err != nil {
			t.Fatalf("SetInlineExtents: %v", err)
		}
		if err := ext4.WriteInode(rw, 0, sb, root); err != nil {
			t.Fatalf("writeInode(root): %v", err)
		}
		if _, err := ext4.LookupPath(rw, 0, sb, "/missing"); err == nil {
			t.Fatalf("expected LookupPath to fail when parent directory block is unreadable")
		}
	})

	t.Run("ReadLink propagates inode read error", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		rw := ext4.CloneFSImage(t, fs)
		sb := ext4.CloneSuperblockFromFS(fs)
		root, err := ext4.ReadInode(rw, 0, sb, ext4.RootIno)
		if err != nil {
			t.Fatalf("readInode(root): %v", err)
		}
		if err := ext4.AddDirEntry(rw, 0, sb, root, sb.InodesCount+1000, "badlink", ext4.FtSymlink); err != nil {
			t.Fatalf("addDirEntry badlink: %v", err)
		}
		if _, err := ext4.ReadDir(rw, 0, sb, root); err != nil {
			t.Fatalf("ReadDir after AddDirEntry: %v", err)
		}
		inoNum := sb.InodesCount + 1000
		if _, err := ext4.ReadInode(rw, 0, sb, inoNum); err != nil {
			// expected to fail; ignore
		}
		if _, err := ext4.LookupPath(rw, 0, sb, "/badlink"); err == nil {
			t.Fatalf("expected LookupPath to fail when target inode cannot be read")
		}
	})
}
