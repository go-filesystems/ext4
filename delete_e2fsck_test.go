package filesystem_ext4

import (
	"encoding/binary"
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"
)

// DeleteFile must leave the image as the kernel would: the freed inode
// scrubbed (no links, a dtime, no blocks), its data and extent-tree blocks
// free in the bitmap, the counts right. e2fsck -fn exits 0 only when the
// image is wholly consistent (e2fsck(8), exit status).
func TestDeleteFileLeavesImageClean(t *testing.T) {
	mke2fs := hostTool("mke2fs")
	fsck := hostTool("e2fsck")
	if mke2fs == "" || fsck == "" {
		t.Skip("e2fsprogs not available")
	}
	img := filepath.Join(t.TempDir(), "img")
	if out, err := exec.Command(mke2fs, "-q", "-t", "ext4", "-O", "metadata_csum,uninit_bg", "-b", "1024",
		"-g", "8192", "-N", "256", "-E", "lazy_itable_init=1", img, "64M").CombinedOutput(); err != nil {
		t.Skipf("mke2fs unusable here: %v\n%s", err, out)
	}
	clean := func(stage string) {
		t.Helper()
		if out, err := exec.Command(fsck, "-fn", img).CombinedOutput(); err != nil {
			t.Fatalf("%s: e2fsck: %v\n%s", stage, err, out)
		}
	}
	clean("fresh image")

	fsys, err := Open(img, -1)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		if err := fsys.WriteFile(fmt.Sprintf("/s%02d", i), pattern(200+i, byte(i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Large files, each filling most of a block group (8 MiB), so the three
	// land in different groups.
	for i := 0; i < 3; i++ {
		if err := fsys.WriteFile(fmt.Sprintf("/big%d", i), pattern(5<<20+i, byte(i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := fsys.WriteFile("/linked", pattern(5000, 9), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fsys.(interface{ Link(string, string) error }).Link("/linked", "/linked2"); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Close(); err != nil {
		t.Fatal(err)
	}
	clean("after create")

	fsys, err = Open(img, -1)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i += 2 {
		if err := fsys.DeleteFile(fmt.Sprintf("/s%02d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := fsys.DeleteFile("/big1"); err != nil {
		t.Fatal(err)
	}
	// One name of a hardlinked file: the inode and its data stay.
	if err := fsys.DeleteFile("/linked"); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Close(); err != nil {
		t.Fatal(err)
	}
	clean("after delete, one link left")

	fsys, err = Open(img, -1)
	if err != nil {
		t.Fatal(err)
	}
	got, err := fsys.ReadFile("/linked2")
	if err != nil || string(got) != string(pattern(5000, 9)) {
		t.Fatalf("surviving link lost its data: %v", err)
	}
	if err := fsys.DeleteFile("/linked2"); err != nil {
		t.Fatal(err)
	}
	if err := fsys.DeleteFile("/big0"); err != nil {
		t.Fatal(err)
	}
	if err := fsys.DeleteFile("/big2"); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Close(); err != nil {
		t.Fatal(err)
	}
	clean("after the last link")

	// The space comes back: a file as large as the ones freed fits again.
	fsys, err = Open(img, -1)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile("/again", pattern(7<<20, 3), 0o644); err != nil {
		t.Fatalf("freed space not reusable: %v", err)
	}
	if err := fsys.Close(); err != nil {
		t.Fatal(err)
	}
	clean("after reuse")
}

func TestScrubFreedInode(t *testing.T) {
	in := &inode{raw: make([]byte, 256)}
	in.raw[inodeOffLinksCount] = 1
	binary.LittleEndian.PutUint32(in.raw[inodeOffFlags:], InodeFlagExtents)
	in.setSize(1 << 33)
	in.setBlocks512(77)
	scrubFreedInode(in, 1234)
	if in.raw[inodeOffLinksCount] != 0 || in.raw[inodeOffDtime] != 0xD2 || in.size != 0 ||
		in.raw[28] != 0 || in.raw[inodeOffBlock] != 0x0A || in.raw[inodeOffBlock+1] != 0xF3 ||
		in.raw[inodeOffBlock+4] != 4 || in.raw[inodeOffBlock+2] != 0 {
		t.Fatalf("not scrubbed: %x", in.raw[:80])
	}
}
