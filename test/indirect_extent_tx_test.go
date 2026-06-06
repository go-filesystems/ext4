package filesystem_ext4_test

import (
	"encoding/binary"
	"os"
	"testing"

	ext4 "github.com/go-filesystems/ext4"
)

// TestIndirectExtentTransaction verifies that adding a dir entry which
// converts inline extents into an indexed (child leaf) extent tree uses the
// journal transaction path so the child leaf and inode updates are committed
// atomically.
func TestIndirectExtentTransaction(t *testing.T) {
	tmp, err := os.CreateTemp("", "ext4indirect")
	if err != nil {
		t.Fatal(err)
	}
	path := tmp.Name()
	tmp.Close()
	defer os.Remove(path)

	fsIfc, err := ext4.Format(path, int64(4096*128), ext4.FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	fs, ok := fsIfc.(*ext4.Ext4FS)
	if !ok {
		t.Fatalf("Format returned unexpected FS type")
	}
	sb := ext4.CloneSuperblockFromFS(fs)

	// Open the backing file directly so we can enable the sidecar journal
	// and pass a plain *os.File to the low-level API used by the codepaths
	// under test.
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open file: %v", err)
	}
	defer f.Close()

	j, err := ext4.OpenJournal(f, 0, int(sb.BlockSize), sb)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	if j == nil {
		t.Fatalf("expected sidecar journal to be enabled for file")
	}

	// Read root inode and force it to have 4 inline extents so that adding
	// one more entry will move the extents into an indexed child leaf.
	root, err := ext4.ReadInode(f, 0, sb, ext4.RootIno)
	if err != nil {
		t.Fatalf("ReadInode(root): %v", err)
	}
	exts := []ext4.ExtentLeaf{
		{LogBlock: 0, PhysBlock: 10, Count: 1},
		{LogBlock: 1, PhysBlock: 11, Count: 1},
		{LogBlock: 2, PhysBlock: 12, Count: 1},
		{LogBlock: 3, PhysBlock: 13, Count: 1},
	}
	if err := ext4.SetInlineExtents(root, exts); err != nil {
		t.Fatalf("SetInlineExtents: %v", err)
	}
	if err := ext4.WriteInode(f, 0, sb, root); err != nil {
		t.Fatalf("WriteInode(root): %v", err)
	}

	// Allocate a new inode to add as a directory entry.
	child, err := ext4.AllocInode(f, 0, sb, false)
	if err != nil {
		t.Fatalf("AllocInode: %v", err)
	}
	in := ext4.NewTestInode(child, uint16(sb.InodeSize))
	ext4.SetMode(in, 0x8000|0o644, 1)
	ext4.SetSize(in, 1)
	if err := ext4.WriteInode(f, 0, sb, in); err != nil {
		t.Fatalf("WriteInode(child): %v", err)
	}

	// This AddDirEntry should trigger the indirect branch and use a
	// Transaction that includes the child leaf block and the inode update.
	if err := ext4.AddDirEntry(f, 0, sb, root, child, "indirect.txt", ext4.FtRegFile); err != nil {
		t.Fatalf("AddDirEntry: %v", err)
	}

	// Re-read the root inode and verify the extent header indicates a depth
	// of 1 (i.e. index -> leaf), showing the conversion to an indexed tree.
	root2, err := ext4.ReadInode(f, 0, sb, ext4.RootIno)
	if err != nil {
		t.Fatalf("ReadInode(root) after AddDirEntry: %v", err)
	}
	raw := ext4.InodeRaw(root2)
	hdr := raw[ext4.InodeOffBlock : ext4.InodeOffBlock+60]
	depth := binary.LittleEndian.Uint16(hdr[6:])
	if depth != 1 {
		t.Fatalf("expected inode extent depth=1 after AddDirEntry, got %d", depth)
	}
}
