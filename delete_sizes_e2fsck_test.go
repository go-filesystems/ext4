package filesystem_ext4

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"
)

// Deleting files of several sizes, including ones larger than a block group
// (8 MiB at 1 KiB blocks with the default 8192 blocks per group) and ones whose
// extent tree needs an index block, must leave the image e2fsck-clean.
func TestDeleteFragmentedFileLeavesImageClean(t *testing.T) {
	mke2fs := hostTool("mke2fs")
	fsck := hostTool("e2fsck")
	if mke2fs == "" || fsck == "" {
		t.Skip("e2fsprogs not available")
	}
	// The holes are 3 blocks, so these need dozens of extents: an index block.
	for _, size := range []int{60 << 10, 120 << 10} {
		t.Run(fmt.Sprintf("%dKiB", size>>10), func(t *testing.T) {
			img := filepath.Join(t.TempDir(), "img")
			if out, err := exec.Command(mke2fs, "-q", "-t", "ext4", "-O", "metadata_csum,uninit_bg",
				"-b", "1024", "-g", "2048", "-N", "512", "-E", "lazy_itable_init=1", img, "32M").CombinedOutput(); err != nil {
				t.Skipf("mke2fs unusable here: %v\n%s", err, out)
			}
			clean := func(stage string) {
				t.Helper()
				if out, err := exec.Command(fsck, "-fn", img).CombinedOutput(); err != nil {
					t.Fatalf("%s: e2fsck: %v\n%s", stage, err, out)
				}
			}
			fsys, err := Open(img, -1)
			if err != nil {
				t.Fatal(err)
			}
			// Checkerboard the free space so a large file needs many extents.
			for i := 0; i < 100; i++ {
				if err := fsys.WriteFile(fmt.Sprintf("/p%03d", i), pattern(3000, byte(i)), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < 100; i += 2 {
				if err := fsys.DeleteFile(fmt.Sprintf("/p%03d", i)); err != nil {
					t.Fatal(err)
				}
			}
			big := pattern(size, 7)
			if err := fsys.WriteFile("/frag", big, 0o644); err != nil {
				t.Skipf("writer cannot place the file here: %v", err)
			}
			if err := fsys.Close(); err != nil {
				t.Fatal(err)
			}
			clean("after create")
			fsys, err = Open(img, -1)
			if err != nil {
				t.Fatal(err)
			}
			if err := fsys.DeleteFile("/frag"); err != nil {
				t.Fatal(err)
			}
			if err := fsys.Close(); err != nil {
				t.Fatal(err)
			}
			clean("after delete")
		})
	}
}

func TestDeleteFileSizesLeaveImageClean(t *testing.T) {
	mke2fs := hostTool("mke2fs")
	fsck := hostTool("e2fsck")
	if mke2fs == "" || fsck == "" {
		t.Skip("e2fsprogs not available")
	}
	for _, bs := range []int{1024, 4096} {
		for _, size := range []int{3 << 20, 8 << 20} {
			t.Run(fmt.Sprintf("bs%d/%dMiB", bs, size>>20), func(t *testing.T) {
				img := filepath.Join(t.TempDir(), "img")
				if out, err := exec.Command(mke2fs, "-q", "-t", "ext4", "-O", "metadata_csum,uninit_bg",
					"-b", fmt.Sprint(bs), "-N", "128", "-E", "lazy_itable_init=1", img, "64M").CombinedOutput(); err != nil {
					t.Skipf("mke2fs unusable here: %v\n%s", err, out)
				}
				clean := func(stage string) {
					t.Helper()
					if out, err := exec.Command(fsck, "-fn", img).CombinedOutput(); err != nil {
						t.Fatalf("%s: e2fsck: %v\n%s", stage, err, out)
					}
				}
				fsys, err := Open(img, -1)
				if err != nil {
					t.Fatal(err)
				}
				if err := fsys.WriteFile("/f", pattern(size, 5), 0o644); err != nil {
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
				if err := fsys.DeleteFile("/f"); err != nil {
					t.Fatal(err)
				}
				if err := fsys.Close(); err != nil {
					t.Fatal(err)
				}
				clean("after delete")
			})
		}
	}
}
