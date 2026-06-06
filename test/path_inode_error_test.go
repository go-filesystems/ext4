package filesystem_ext4_test

import (
	"encoding/binary"
	"errors"
	"testing"

	ext4 "github.com/go-filesystems/ext4"
)

// memFile is provided by shared test helpers in test_helpers_test.go
func TestLookupPathAndSymlinkAdditionalPaths(t *testing.T) {
	errBoom := errors.New("boom")

	t.Run("readDir extents error", func(t *testing.T) {
		sb := &ext4.Superblock{BlockSize: 1024}
		badDir := ext4.NewTestInode(ext4.RootIno, 256)
		if _, err := ext4.ReadDir(&memFile{buf: make([]byte, 4096)}, 0, sb, badDir); err == nil {
			t.Fatalf("expected readDir to fail when extents are unavailable")
		}
	})

	t.Run("lookupPath root inode read error", func(t *testing.T) {
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

		if _, err := ext4.LookupPath(rw, 0, sb, "/"); err == nil {
			t.Fatalf("expected lookupPath to fail when reading root inode")
		}
	})

	t.Run("lookupPath directory block read error", func(t *testing.T) {
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

		if _, err := ext4.LookupPath(rw, 0, sb, "/missing"); err == nil {
			t.Fatalf("expected lookupPath to fail when reading directory data")
		}
	})

	t.Run("lookupPath child inode read error", func(t *testing.T) {
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

		if _, err := ext4.LookupPath(rw, 0, sb, "/foo"); err == nil {
			t.Fatalf("expected lookupPath to fail when reading child inode")
		}
	})

	t.Run("lookupPath resolves symlink before final component", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()

		if err := fs.MkDir("/dir", 0o755); err != nil {
			t.Fatalf("MkDir /dir: %v", err)
		}
		if err := fs.MkDir("/dir/sub", 0o755); err != nil {
			t.Fatalf("MkDir /dir/sub: %v", err)
		}
		if err := fs.WriteFile("/dir/sub/file.txt", []byte("payload"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		ext4.AddFastSymlink(t, fs, "/", "absdir", "/dir")
		ext4.AddFastSymlink(t, fs, "/", "reldir", "dir")

		for _, path := range []string{"/absdir/sub/file.txt", "/reldir/sub/file.txt"} {
			got, err := fs.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile(%q): %v", path, err)
			}
			if string(got) != "payload" {
				t.Fatalf("ReadFile(%q) = %q, want payload", path, string(got))
			}
		}
	})

	t.Run("lookupParent rejects invalid path", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		rw := ext4.CloneFSImage(t, fs)
		sb := ext4.CloneSuperblockFromFS(fs)
		if _, _, err := ext4.LookupParent(rw, 0, sb, "/"); err == nil {
			t.Fatalf("expected lookupParent to reject root path")
		}
	})

	t.Run("slow symlink read propagates block error", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		rw := ext4.CloneFSImage(t, fs)
		sb := ext4.CloneSuperblockFromFS(fs)

		longTarget := make([]byte, 128)
		for i := range longTarget {
			longTarget[i] = 'a' + byte(i%26)
		}
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

		rw.ReadHook = func(off int64, _ []byte) error {
			if off == int64(phys[0])*int64(sb.BlockSize) {
				return errBoom
			}
			return nil
		}

		if _, err := ext4.ReadSymlink(rw, 0, sb, linkInode); err == nil {
			t.Fatalf("expected readSymlink to fail when reading slow-target data")
		}
	})

	t.Run("addDirEntry extents error", func(t *testing.T) {
		sb := &ext4.Superblock{BlockSize: 1024}
		badDir := ext4.NewTestInode(ext4.RootIno, 256)
		if err := ext4.AddDirEntry(&memFile{buf: make([]byte, 4096)}, 0, sb, badDir, 12, "x", ext4.FtRegFile); err == nil {
			t.Fatalf("expected addDirEntry to fail without extents")
		}
	})

	t.Run("addDirEntry directory block read error", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()

		rw := ext4.CloneFSImage(t, fs)
		sb := ext4.CloneSuperblockFromFS(fs)
		root, err := ext4.ReadInode(rw, 0, sb, ext4.RootIno)
		if err != nil {
			t.Fatalf("readInode(root): %v", err)
		}
		rootBlock := ext4.RootDirBlockFor(t, rw, sb)
		rw.ReadHook = func(off int64, _ []byte) error {
			if off == int64(rootBlock)*int64(sb.BlockSize) {
				return errBoom
			}
			return nil
		}
		if err := ext4.AddDirEntry(rw, 0, sb, root, 12, "x", ext4.FtRegFile); err == nil {
			t.Fatalf("expected addDirEntry to fail when reading directory block")
		}
	})
}

func TestInodeHelperErrorPaths(t *testing.T) {
	errBoom := errors.New("boom")

	t.Run("writeInode propagates BGD read error", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		rw := ext4.CloneFSImage(t, fs)
		sb := ext4.CloneSuperblockFromFS(fs)
		root, err := ext4.ReadInode(rw, 0, sb, ext4.RootIno)
		if err != nil {
			t.Fatalf("readInode(root): %v", err)
		}
		rw.ReadHook = func(off int64, _ []byte) error {
			if off == ext4.BgdOffset(sb, 0) {
				return errBoom
			}
			return nil
		}
		if err := ext4.WriteInode(rw, 0, sb, root); err == nil {
			t.Fatalf("expected writeInode to fail when reading BGD")
		}
	})

	t.Run("writeInode propagates inode write error", func(t *testing.T) {
		fs, cleanup := ext4.NewTempFS(t)
		defer cleanup()
		rw := ext4.CloneFSImage(t, fs)
		sb := ext4.CloneSuperblockFromFS(fs)
		root, err := ext4.ReadInode(rw, 0, sb, ext4.RootIno)
		if err != nil {
			t.Fatalf("readInode(root): %v", err)
		}
		rootOff := ext4.InodeOffsetFor(rw, sb, ext4.RootIno)
		rw.WriteHook = func(off int64, _ []byte) error {
			if off == rootOff {
				return errBoom
			}
			return nil
		}
		if err := ext4.WriteInode(rw, 0, sb, root); err == nil {
			t.Fatalf("expected writeInode to fail on inode write")
		}
	})

	t.Run("parseExtentNode stops on truncated entry list", func(t *testing.T) {
		buf := make([]byte, 24)
		le := binary.LittleEndian
		le.PutUint16(buf[0:], ext4.ExtentMagic)
		le.PutUint16(buf[2:], 2)
		le.PutUint16(buf[6:], 0)
		le.PutUint32(buf[12:], 0)
		le.PutUint16(buf[16:], 1)
		le.PutUint32(buf[20:], 9)

		exts, err := ext4.ParseExtentNode(&memFile{buf: make([]byte, 4096)}, 0, &ext4.Superblock{BlockSize: 1024}, buf, 17, nil)
		if err != nil {
			t.Fatalf("parseExtentNode: %v", err)
		}
		if len(exts) != 1 {
			t.Fatalf("len(exts) = %d, want 1", len(exts))
		}
	})

	t.Run("parseExtentNode child read error", func(t *testing.T) {
		buf := make([]byte, 24)
		le := binary.LittleEndian
		le.PutUint16(buf[0:], ext4.ExtentMagic)
		le.PutUint16(buf[2:], 1)
		le.PutUint16(buf[6:], 1)
		le.PutUint32(buf[16:], 7)

		rw := ext4.NewHookMemFile(make([]byte, 4096*8))
		rw.ReadHook = func(off int64, _ []byte) error {
			if off == 7*4096 {
				return errBoom
			}
			return nil
		}
		if _, err := ext4.ParseExtentNode(rw, 0, &ext4.Superblock{BlockSize: 4096}, buf, 23, nil); err == nil {
			t.Fatalf("expected parseExtentNode to fail when reading child block")
		}
	})

	t.Run("parseExtentNode child parse error", func(t *testing.T) {
		buf := make([]byte, 24)
		le := binary.LittleEndian
		le.PutUint16(buf[0:], ext4.ExtentMagic)
		le.PutUint16(buf[2:], 1)
		le.PutUint16(buf[6:], 1)
		le.PutUint32(buf[16:], 7)

		rw := ext4.NewHookMemFile(make([]byte, 4096*8))
		if _, err := ext4.ParseExtentNode(rw, 0, &ext4.Superblock{BlockSize: 4096}, buf, 23, nil); err == nil {
			t.Fatalf("expected parseExtentNode to fail on invalid child node")
		}
	})

	t.Run("readFileData extents error", func(t *testing.T) {
		in := ext4.NewTestInode(1, 256)
		if _, err := ext4.ReadFileData(&memFile{buf: make([]byte, 4096)}, 0, &ext4.Superblock{BlockSize: 1024}, in); err == nil {
			t.Fatalf("expected readFileData to fail without extents")
		}
	})

	t.Run("readFileData block read error", func(t *testing.T) {
		sb := &ext4.Superblock{BlockSize: 1024}
		in := ext4.NewTestInode(1, 256)
		ext4.SetMode(in, 0x8000, 1)
		ext4.SetSize(in, 16)
		if err := ext4.SetInlineExtents(in, []ext4.ExtentLeaf{{LogBlock: 0, PhysBlock: 3, Count: 1}}); err != nil {
			t.Fatalf("SetInlineExtents: %v", err)
		}
		rw := ext4.NewHookMemFile(make([]byte, 4096))
		rw.ReadHook = func(off int64, _ []byte) error {
			if off == 3*1024 {
				return errBoom
			}
			return nil
		}
		if _, err := ext4.ReadFileData(rw, 0, sb, in); err == nil {
			t.Fatalf("expected readFileData to fail on block read")
		}
	})

	t.Run("readFileData stops at file size", func(t *testing.T) {
		sb := &ext4.Superblock{BlockSize: 1024}
		in := ext4.NewTestInode(1, 256)
		ext4.SetMode(in, 0x8000, 1)
		ext4.SetSize(in, 1024)
		if err := ext4.SetInlineExtents(in, []ext4.ExtentLeaf{{LogBlock: 0, PhysBlock: 1, Count: 2}}); err != nil {
			t.Fatalf("SetInlineExtents: %v", err)
		}
		rw := ext4.NewHookMemFile(make([]byte, 4096))
		b := ext4.HookMemFileBuf(rw)
		for i := 0; i < 1024; i++ {
			b[1024+i] = 'x'
			b[2048+i] = 'y'
		}
		got, err := ext4.ReadFileData(rw, 0, sb, in)
		if err != nil {
			t.Fatalf("readFileData: %v", err)
		}
		if len(got) != 1024 {
			t.Fatalf("len(got) = %d, want 1024", len(got))
		}
	})
}
