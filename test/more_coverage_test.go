package filesystem_ext4_test

import (
	"encoding/binary"
	"os"
	"testing"

	ext4 "github.com/go-filesystems/ext4"
)

// memReaderAt provided by shared test helpers in test_helpers_test.go

// addFastSymlink delegates to the exported test helper in the package.
func addFastSymlink(t *testing.T, fs *ext4.Ext4FS, parentPath, name, target string) {
	t.Helper()
	ext4.AddFastSymlink(t, fs, parentPath, name, target)
}

func TestPartitionOffsetAdditionalEdges(t *testing.T) {
	t.Run("gpt short entry size", func(t *testing.T) {
		le := binary.LittleEndian
		buf := make([]byte, 64*1024)
		hdr := buf[512:]
		copy(hdr[:8], "EFI PART")
		le.PutUint64(hdr[72:], 2)
		le.PutUint32(hdr[80:], 1)
		le.PutUint32(hdr[84:], 64)
		if _, err := ext4.PartitionOffset(memReaderAt(buf), -1); err == nil {
			t.Fatalf("expected GPT short entry size error")
		}
	})

	t.Run("gpt no linux partition", func(t *testing.T) {
		le := binary.LittleEndian
		buf := make([]byte, 64*1024)
		hdr := buf[512:]
		copy(hdr[:8], "EFI PART")
		le.PutUint64(hdr[72:], 2)
		le.PutUint32(hdr[80:], 1)
		le.PutUint32(hdr[84:], 128)
		entry := buf[1024:]
		copy(entry[:16], []byte{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1})
		le.PutUint64(entry[32:], 2048)
		if _, err := ext4.PartitionOffset(memReaderAt(buf), -1); err == nil {
			t.Fatalf("expected GPT no Linux partition error")
		}
	})

	t.Run("mbr explicit missing index", func(t *testing.T) {
		le := binary.LittleEndian
		buf := make([]byte, 4096)
		buf[510] = 0x55
		buf[511] = 0xAA
		entry := buf[446:]
		entry[4] = 0x83
		le.PutUint32(entry[8:], 2048)
		if _, err := ext4.PartitionOffset(memReaderAt(buf), 3); err == nil {
			t.Fatalf("expected MBR missing index error")
		}
	})
}

func TestOpenAdditionalErrors(t *testing.T) {
	if _, err := ext4.Open("/definitely/missing/ext4.img", -1); err == nil {
		t.Fatalf("expected Open to fail for missing file")
	}

	t.Run("bad superblock", func(t *testing.T) {
		path := t.TempDir() + "/bad-super.img"
		if err := os.WriteFile(path, make([]byte, 4096), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if _, err := ext4.Open(path, -1); err == nil {
			t.Fatalf("expected Open to fail for non-ext4 image")
		}
	})

	t.Run("invalid partition index", func(t *testing.T) {
		le := binary.LittleEndian
		path := t.TempDir() + "/bad-partition.img"
		buf := make([]byte, 4096)
		buf[510] = 0x55
		buf[511] = 0xAA
		entry := buf[446:]
		entry[4] = 0x83
		le.PutUint32(entry[8:], 2048)
		if err := os.WriteFile(path, buf, 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if _, err := ext4.Open(path, 2); err == nil {
			t.Fatalf("expected Open to fail for invalid partition index")
		}
	})
}

func TestReadSuperblockFallbacksAndErrors(t *testing.T) {
	mf := ext4.NewHookMemFile(make([]byte, 64*1024))
	le := binary.LittleEndian

	raw := make([]byte, 1024)
	le.PutUint16(raw[56:], 0xEF53)
	le.PutUint32(raw[0:], 32)
	le.PutUint32(raw[4:], 8192)
	le.PutUint32(raw[12:], 100)
	le.PutUint32(raw[16:], 20)
	le.PutUint32(raw[20:], 0)
	le.PutUint32(raw[24:], 2)
	le.PutUint32(raw[32:], 8192)
	le.PutUint32(raw[40:], 128)
	copy(ext4.HookMemFileBuf(mf)[1024:], raw)

	sb, err := ext4.ReadSuperblock(mf, 0)
	if err != nil {
		t.Fatalf("readSuperblock: %v", err)
	}
	if sb.InodeSize != 128 {
		t.Fatalf("InodeSize = %d, want 128 fallback", sb.InodeSize)
	}
	if sb.DescSize != 32 {
		t.Fatalf("DescSize = %d, want 32 fallback", sb.DescSize)
	}

	copy(ext4.HookMemFileBuf(mf)[1024:], make([]byte, 1024))
	if _, err := ext4.ReadSuperblock(mf, 0); err == nil {
		t.Fatalf("expected bad magic error")
	}
}

func TestSymlinkResolutionAbsoluteAndRelative(t *testing.T) {
	fs, cleanup := ext4.NewTempFS(t)
	defer cleanup()

	if err := fs.MkDir("/dir", 0o755); err != nil {
		t.Fatalf("MkDir /dir: %v", err)
	}
	if err := fs.WriteFile("/dir/file.txt", []byte("payload"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	addFastSymlink(t, fs, "/dir", "rel", "file.txt")
	addFastSymlink(t, fs, "/", "abs", "/dir/file.txt")

	relData, err := fs.ReadFile("/dir/rel")
	if err != nil {
		t.Fatalf("ReadFile relative symlink: %v", err)
	}
	if string(relData) != "payload" {
		t.Fatalf("relative symlink content = %q, want %q", string(relData), "payload")
	}

	absData, err := fs.ReadFile("/abs")
	if err != nil {
		t.Fatalf("ReadFile absolute symlink: %v", err)
	}
	if string(absData) != "payload" {
		t.Fatalf("absolute symlink content = %q, want %q", string(absData), "payload")
	}

	target, err := fs.ReadLink("/abs")
	if err != nil {
		t.Fatalf("ReadLink /abs: %v", err)
	}
	if target != "/dir/file.txt" {
		t.Fatalf("ReadLink target = %q, want %q", target, "/dir/file.txt")
	}
}

func TestFilesystemTypeErrors(t *testing.T) {
	fs, cleanup := ext4.NewTempFS(t)
	defer cleanup()

	if err := fs.MkDir("/dir", 0o755); err != nil {
		t.Fatalf("MkDir /dir: %v", err)
	}
	if err := fs.WriteFile("/file", []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile /file: %v", err)
	}

	if _, err := fs.ReadFile("/dir"); err == nil {
		t.Fatalf("expected ReadFile on directory to fail")
	}
	if _, err := fs.ListDir("/file"); err == nil {
		t.Fatalf("expected ListDir on regular file to fail")
	}
}

func TestRenameReplacementScenarios(t *testing.T) {
	fs, cleanup := ext4.NewTempFS(t)
	defer cleanup()

	if err := fs.WriteFile("/src.txt", []byte("src"), 0o644); err != nil {
		t.Fatalf("WriteFile /src.txt: %v", err)
	}
	if err := fs.WriteFile("/dst.txt", []byte("dst"), 0o644); err != nil {
		t.Fatalf("WriteFile /dst.txt: %v", err)
	}
	if err := fs.Rename("/src.txt", "/dst.txt"); err != nil {
		t.Fatalf("Rename file replacement: %v", err)
	}
	got, err := fs.ReadFile("/dst.txt")
	if err != nil {
		t.Fatalf("ReadFile /dst.txt: %v", err)
	}
	if string(got) != "src" {
		t.Fatalf("replacement content = %q, want %q", string(got), "src")
	}
}

func TestParseExtentNodeMasksUninitializedLength(t *testing.T) {
	buf := make([]byte, 24)
	le := binary.LittleEndian
	le.PutUint16(buf[0:], ext4.ExtentMagic)
	le.PutUint16(buf[2:], 1)
	le.PutUint16(buf[6:], 0)
	le.PutUint32(buf[12:], 0)
	le.PutUint16(buf[16:], 32769)
	le.PutUint16(buf[18:], 0)
	le.PutUint32(buf[20:], 123)

	exts, err := ext4.ParseExtentNode(ext4.NewHookMemFile(make([]byte, 4096)), 0, &ext4.Superblock{BlockSize: 1024}, buf, 7, nil)
	if err != nil {
		t.Fatalf("parseExtentNode: %v", err)
	}
	if len(exts) != 1 || exts[0].Count != 1 {
		t.Fatalf("unexpected extents: %+v", exts)
	}
}
