package filesystem_ext4

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// AUDIT (opus, 2026-11-22): on a metadata_csum image (ext4_4k.img, built by
// mke2fs -O metadata_csum), the directory-block checksum tail is written but never
// verified on read. A name rewritten in the root directory block, with the tail
// left as it was, is listed as genuine; the kernel refuses the block (EFSBADCRC)
// and e2fsck reports "Directory block checksum does not match".
func TestAuditOpusDirBlockChecksumNotVerified(t *testing.T) {
	src := filepath.Join(fixtureDir(t), "ext4_4k.img")
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	orig, forged := []byte("fast.lnk"), []byte("evil.lnk")
	n := bytes.Count(raw, orig)
	if n == 0 {
		t.Fatal("name not found in image")
	}
	raw = bytes.ReplaceAll(raw, orig, forged)
	img := filepath.Join(t.TempDir(), "img")
	if err := os.WriteFile(img, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	fsys, err := Open(img, -1)
	if err != nil {
		t.Logf("refused on open (expected): %v", err)
		return
	}
	defer fsys.Close()
	ents, err := fsys.ListDir("/")
	if err != nil {
		t.Logf("refused on list (expected): %v", err)
		return
	}
	for _, e := range ents {
		if e.Name() == string(forged) {
			t.Errorf("dir block csum not verified: %d occurrence(s) tampered, ListDir returns %q", n, e.Name())
		}
	}
}
