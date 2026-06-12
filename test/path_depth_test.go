package filesystem_ext4_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	ext4 "github.com/go-filesystems/ext4"
)

// TestDeepPath_Read is a regression test for a path resolver that conflated
// directory-descent recursion with symlink-hop counting. lookupPathFrom used a
// single counter bumped on every component descent and rejected anything past 8
// with a bogus "symlink loop" error — so a sufficiently deep, symlink-free tree
// could not be read at all.
//
// The bug lives in the resolver, not in any particular on-disk layout, so we
// reproduce it against both a block-map image (ext2) and an extents image
// (ext4): reading a file nested far deeper than the old cap must succeed with
// zero symlinks involved.
func TestDeepPath_Read(t *testing.T) {
	mke2fs := findTool("mke2fs")
	if mke2fs == "" {
		t.Skip("mke2fs not available — skipping deep-path test")
	}

	// Build a tree d00/d01/.../d11/leaf.txt — 12 directory levels, well past
	// the old depth>8 cap, with no symlinks anywhere on the path.
	const levels = 12
	want := []byte("leaf at the bottom of a deep tree\n")
	src := t.TempDir()
	dir := src
	var comps []string
	for i := 0; i < levels; i++ {
		comp := fmt.Sprintf("d%02d", i)
		comps = append(comps, comp)
		dir = filepath.Join(dir, comp)
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "leaf.txt"), want, 0o644); err != nil {
		t.Fatal(err)
	}
	deepPath := "/" + strings.Join(comps, "/") + "/leaf.txt"

	for _, fs := range []struct {
		name string
		args []string // mke2fs args before "-d src img"
	}{
		{"ext2-blockmap", []string{"-t", "ext2", "-b", "1024"}},
		{"ext4-extents", []string{"-t", "ext4", "-O", "extents,metadata_csum,^inline_data,^huge_file"}},
	} {
		t.Run(fs.name, func(t *testing.T) {
			img := filepath.Join(t.TempDir(), "fs.img")
			f, err := os.Create(img)
			if err != nil {
				t.Fatal(err)
			}
			f.Truncate(16 * 1024 * 1024)
			f.Close()

			args := append([]string{}, fs.args...)
			args = append(args, "-F", "-L", "test", "-d", src, img)
			if out, err := exec.Command(mke2fs, args...).CombinedOutput(); err != nil {
				t.Fatalf("mke2fs failed: %v\n%s", err, out)
			}

			img4, err := ext4.Open(img, -1)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer img4.Close()

			got, err := img4.ReadFile(deepPath)
			if err != nil {
				t.Fatalf("ReadFile(%s): %v", deepPath, err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("ReadFile(%s) = %q, want %q", deepPath, got, want)
			}
		})
	}
}
