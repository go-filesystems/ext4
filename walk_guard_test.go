package filesystem_ext4

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-volumes/safeio"
)

// TestDeleteDirCycleIsRefused: a directory entry of type directory naming an
// ANCESTOR inode (a directory hard link: Link refuses to create one, a hostile
// image contains one) used to make DeleteDir walk /a/b/up/b/up/... for ever
// while holding the fs lock. It is now refused before anything is changed.
func TestDeleteDirCycleIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fsi, err := Format(path, 4096*4096, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	if err := fsi.MkDir("/a", 0o755); err != nil {
		t.Fatalf("MkDir(/a): %v", err)
	}
	if err := fsi.MkDir("/a/b", 0o755); err != nil {
		t.Fatalf("MkDir(/a/b): %v", err)
	}
	w := fsi.(*ext4FS)
	rw := getRW(w)
	aIno, err := lookupPath(rw, w.partOffset, w.sb, "/a")
	if err != nil {
		t.Fatalf("lookupPath(/a): %v", err)
	}
	bIno, err := lookupPath(rw, w.partOffset, w.sb, "/a/b")
	if err != nil {
		t.Fatalf("lookupPath(/a/b): %v", err)
	}
	if err := addDirEntry(rw, w.partOffset, w.sb, bIno, aIno.num, "up", FtDir); err != nil {
		t.Fatalf("addDirEntry: %v", err)
	}
	if err := fsi.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()
	if _, err := fs.ListDir("/a/b/up/b"); err != nil {
		t.Fatalf("ListDir(/a/b/up/b): %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- fs.DeleteDir("/a") }()
	select {
	case err := <-done:
		if !errors.Is(err, safeio.ErrCycle) {
			t.Fatalf("DeleteDir(/a) = %v, want safeio.ErrCycle", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("DeleteDir(/a) still running after 20s: the cycle is not refused")
	}
	if _, err := fs.ListDir("/a/b"); err != nil {
		t.Fatalf("directory gone after a refused delete: %v", err)
	}
}

// A directory entry naming an inode the image cannot hold is an error of the
// check (DeleteDir refuses, nothing is changed), not a pass.
func TestDeleteDirUnreadableChildIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fsi, err := Format(path, 4096*4096, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fsi.Close()
	if err := fsi.MkDir("/a", 0o755); err != nil {
		t.Fatalf("MkDir(/a): %v", err)
	}
	w := fsi.(*ext4FS)
	rw := getRW(w)
	aIno, err := lookupPath(rw, w.partOffset, w.sb, "/a")
	if err != nil {
		t.Fatalf("lookupPath(/a): %v", err)
	}
	if err := addDirEntry(rw, w.partOffset, w.sb, aIno, 0x7FFFFFF0, "ghost", FtDir); err != nil {
		t.Fatalf("addDirEntry: %v", err)
	}
	if err := checkDirTree(rw, w.partOffset, w.sb, aIno); err == nil {
		t.Fatal("checkDirTree with a child directory outside the inode table = nil, want an error")
	}
	if err := fsi.DeleteDir("/a"); err == nil {
		t.Fatal("DeleteDir(/a) with an unreadable child directory = nil, want an error")
	}
	if _, err := fsi.ListDir("/a"); err != nil {
		t.Fatalf("directory gone after a refused delete: %v", err)
	}
}

// A directory whose own blocks cannot be read is an error of the check.
func TestCheckDirTreeReportsUnreadableDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fsi, err := Format(path, 4096*4096, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fsi.Close()
	if err := fsi.MkDir("/a", 0o755); err != nil {
		t.Fatalf("MkDir(/a): %v", err)
	}
	w := fsi.(*ext4FS)
	rw := getRW(w)
	aIno, err := lookupPath(rw, w.partOffset, w.sb, "/a")
	if err != nil {
		t.Fatalf("lookupPath(/a): %v", err)
	}
	bad := *aIno
	bad.raw = append([]byte(nil), aIno.raw...)
	for i := 0; i < 12; i++ { // the extent header: magic, entries, depth
		bad.raw[inodeOffBlock+i] = 0xFF
	}
	if err := checkDirTree(rw, w.partOffset, w.sb, &bad); err == nil {
		t.Fatal("checkDirTree of a directory with a corrupt extent header = nil, want an error")
	}
}
