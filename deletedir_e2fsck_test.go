package filesystem_ext4

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

// DeleteDir must leave the image as the kernel's rmdir does: each removed
// directory's inode scrubbed, its blocks free, bg_used_dirs_count and the
// parent's link count decremented, every checksum right. e2fsck -fn exits 0
// only when the image is wholly consistent.
func TestDeleteDirLeavesImageClean(t *testing.T) {
	mke2fs := hostTool("mke2fs")
	fsck := hostTool("e2fsck")
	if mke2fs == "" || fsck == "" {
		t.Skip("e2fsprogs not available")
	}
	img := filepath.Join(t.TempDir(), "img")
	if out, err := exec.Command(mke2fs, "-q", "-t", "ext4", "-O", "metadata_csum,uninit_bg", "-b", "1024",
		"-g", "8192", "-N", "256", "-E", "lazy_itable_init=1", img, "32M").CombinedOutput(); err != nil {
		t.Skipf("mke2fs unusable here: %v\n%s", err, out)
	}
	clean := func(stage string) {
		t.Helper()
		if out, err := exec.Command(fsck, "-fn", img).CombinedOutput(); err != nil {
			t.Fatalf("%s: e2fsck: %v\n%s", stage, err, out)
		}
	}
	open := func() filesystem.Filesystem {
		t.Helper()
		fsys, err := Open(img, -1)
		if err != nil {
			t.Fatal(err)
		}
		return fsys
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	clean("fresh image")

	fsys := open()
	must(fsys.MkDir("/keep", 0o755))
	must(fsys.MkDir("/d", 0o755))
	must(fsys.MkDir("/d/sub", 0o755))
	must(fsys.MkDir("/d/sub/deeper", 0o755))
	must(fsys.WriteFile("/d/a", pattern(3000, 1), 0o644))
	must(fsys.WriteFile("/d/sub/b", pattern(40000, 2), 0o644))
	must(fsys.WriteFile("/keep/k", pattern(10, 3), 0o644))
	// A directory with enough entries to own several blocks.
	must(fsys.MkDir("/wide", 0o755))
	for i := 0; i < 120; i++ {
		must(fsys.WriteFile(fmt.Sprintf("/wide/file-with-a-long-name-%04d", i), pattern(20, byte(i)), 0o644))
	}
	must(fsys.Close())
	clean("after create")

	fsys = open()
	must(fsys.DeleteDir("/d"))
	must(fsys.Close())
	clean("after DeleteDir /d")

	fsys = open()
	must(fsys.DeleteDir("/wide"))
	must(fsys.Close())
	clean("after DeleteDir /wide")

	// The inodes and blocks come back: the same tree can be built again.
	fsys = open()
	must(fsys.MkDir("/d", 0o755))
	must(fsys.MkDir("/d/sub", 0o755))
	must(fsys.WriteFile("/d/sub/b", pattern(40000, 2), 0o644))
	got, err := fsys.ReadFile("/keep/k")
	if err != nil || string(got) != string(pattern(10, 3)) {
		t.Fatalf("a sibling lost its data: %v", err)
	}
	must(fsys.DeleteDir("/d"))
	must(fsys.DeleteDir("/keep"))
	must(fsys.Close())
	clean("after reuse and removing the rest")
}
