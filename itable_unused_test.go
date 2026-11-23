package filesystem_ext4

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// bg_itable_unused and the INODE_UNINIT flag (ext4#50). Reference:
// docs.kernel.org/filesystems/ext4/globals.html (block group descriptors) and
// fs/ext4/ialloc.c (ext4_new_inode).

func TestNoteInodeAllocated(t *testing.T) {
	csb := &superblock{FeatureROCompat: FeatROCompatMetadataCsum, InodesPerGroup: 32}
	plain := &superblock{InodesPerGroup: 32}

	d := &bgd{Flags: bgInodeUninit | bgItableZeroed, ItableUnused: 21} // inodes 0..10 in use
	d.noteInodeAllocated(csb, 5)                                       // below the water mark
	if d.ItableUnused != 21 || d.Flags&bgInodeUninit != 0 || d.Flags&bgItableZeroed == 0 {
		t.Fatalf("below the mark: unused %d flags %#x", d.ItableUnused, d.Flags)
	}
	d.noteInodeAllocated(csb, 11) // the first never-used inode
	if d.ItableUnused != 20 {
		t.Fatalf("bit 11: unused %d, want 20", d.ItableUnused)
	}
	d.noteInodeAllocated(csb, 31) // the last inode
	if d.ItableUnused != 0 {
		t.Fatalf("bit 31: unused %d, want 0", d.ItableUnused)
	}
	d.noteInodeAllocated(csb, 32) // out of range: untouched
	d.noteInodeAllocated(csb, -1)
	if d.ItableUnused != 0 {
		t.Fatalf("out of range moved the mark: %d", d.ItableUnused)
	}

	// A count above the group (a corrupt descriptor) must not be "lowered" by
	// a bit that names no inode.
	o := &bgd{ItableUnused: 40}
	o.noteInodeAllocated(csb, -1)
	if o.ItableUnused != 40 {
		t.Fatalf("bit -1 lowered the count to %d", o.ItableUnused)
	}

	p := &bgd{Flags: bgInodeUninit, ItableUnused: 7}
	p.noteInodeAllocated(plain, 30)
	if p.ItableUnused != 7 || p.Flags != bgInodeUninit {
		t.Fatalf("a filesystem without uninit_bg was touched: unused %d flags %#x", p.ItableUnused, p.Flags)
	}
	if p.inodeUninit(plain) || !(&bgd{Flags: bgInodeUninit}).inodeUninit(csb) {
		t.Fatal("inodeUninit must follow the feature and the flag")
	}
}

func TestBGDRoundTripsUnusedAndFlags(t *testing.T) {
	for _, tc := range []struct {
		name string
		sb   *superblock
		raw  int
		want uint32
	}{
		{"32-byte", &superblock{FeatureROCompat: FeatROCompatMetadataCsum, DescSize: 32}, 32, 0xBEEF},
		{"64-byte", &superblock{FeatureROCompat: FeatROCompatMetadataCsum, FeatureIncompat: FeatIncompat64bit, DescSize: 64}, 64, 0x1BEEF},
	} {
		d := &bgd{raw: make([]byte, tc.raw), Flags: bgInodeUninit | bgBlockUninit, ItableUnused: tc.want}
		d.encode(tc.sb)
		got := decodeBGD(d.raw, tc.sb)
		if got.ItableUnused != tc.want&map32(tc.raw) || got.Flags != d.Flags {
			t.Fatalf("%s: unused %#x flags %#x, want %#x %#x", tc.name, got.ItableUnused, got.Flags, tc.want&map32(tc.raw), d.Flags)
		}
	}
}

// map32 masks a value to what a descriptor of rawLen bytes can carry.
func map32(rawLen int) uint32 {
	if rawLen >= 64 {
		return 0xFFFFFFFF
	}
	return 0xFFFF
}

// An image this package writes must leave e2fsck no complaint about the
// unused-inode count, across several groups, after create, delete and growth.
func TestWriterKeepsItableUnusedConsistent(t *testing.T) {
	mke2fs := hostTool("mke2fs")
	fsck := hostTool("e2fsck")
	if mke2fs == "" || fsck == "" {
		t.Skip("e2fsprogs not available")
	}
	img := filepath.Join(t.TempDir(), "img")
	// 8 groups of 32 inodes: allocations spread over several groups.
	if out, err := exec.Command(mke2fs, "-q", "-t", "ext4", "-O", "metadata_csum,uninit_bg", "-b", "1024",
		"-g", "1024", "-N", "256", "-E", "lazy_itable_init=1", img, "8M").CombinedOutput(); err != nil {
		t.Skipf("mke2fs unusable here: %v\n%s", err, out)
	}
	// The group bookkeeping this package owns: the unused-inode count, the
	// uninit flags and the checksums. Whether a deleted file's inode is
	// scrubbed is a separate matter, so the delete stage does not judge the
	// bitmaps' agreement with the inodes.
	check := func(stage string, bitmaps bool) {
		t.Helper()
		out, _ := exec.Command(fsck, "-fn", img).CombinedOutput()
		low := strings.ToLower(strings.ReplaceAll(string(out), img, "IMG"))
		bad := []string{"unused inodes", "itable_unused", "uninit", "checksum", "padding"}
		if bitmaps {
			bad = append(bad, "bitmap differences", "free blocks count wrong", "free inodes count wrong")
		}
		for _, b := range bad {
			if strings.Contains(low, b) {
				t.Fatalf("%s: e2fsck complains (%q):\n%s", stage, b, out)
			}
		}
	}
	check("fresh image", true)

	fsys, err := Open(img, -1)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 60; i++ {
		if err := fsys.WriteFile(fmt.Sprintf("/f%02d", i), pattern(300+i, byte(i)), 0o644); err != nil {
			t.Fatalf("create f%02d: %v", i, err)
		}
	}
	for i := 0; i < 6; i++ {
		if err := fsys.MkDir(fmt.Sprintf("/d%d", i), 0o755); err != nil {
			t.Fatalf("mkdir d%d: %v", i, err)
		}
	}
	if err := fsys.Close(); err != nil {
		t.Fatal(err)
	}
	check("after create", true)

	fsys, err = Open(img, -1)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 60; i += 3 {
		if err := fsys.DeleteFile(fmt.Sprintf("/f%02d", i)); err != nil {
			t.Fatalf("delete f%02d: %v", i, err)
		}
	}
	if err := fsys.Close(); err != nil {
		t.Fatal(err)
	}
	check("after delete", false)

	fsys, err = Open(img, -1)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ { // reuse freed inodes, then keep growing past the old mark
		if err := fsys.WriteFile(fmt.Sprintf("/g%02d", i), pattern(5000, byte(i)), 0o644); err != nil {
			t.Fatalf("growth g%02d: %v", i, err)
		}
	}
	if err := fsys.Close(); err != nil {
		t.Fatal(err)
	}
	check("after growth", false)
}
