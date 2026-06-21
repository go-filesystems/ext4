package filesystem_ext4

import (
	"encoding/binary"
	"os"
	"testing"
	"time"

	filesystem "github.com/go-filesystems/interface"
)

// readInodeFields re-reads path's inode and returns its raw uid, gid, atime,
// mtime, ctime for assertions (Stat only exposes mode/size/inode).
func readInodeFields(t *testing.T, fs *ext4FS, path string) (uid, gid, atime, mtime, ctime uint32, mode uint16) {
	t.Helper()
	rw := getRW(fs)
	in, err := lookupPath(rw, fs.partOffset, fs.sb, path)
	if err != nil {
		t.Fatalf("lookupPath %q: %v", path, err)
	}
	le := binary.LittleEndian
	uid = uint32(le.Uint16(in.raw[inodeOffUID:])) | uint32(le.Uint16(in.raw[inodeOffUIDHigh:]))<<16
	gid = uint32(le.Uint16(in.raw[inodeOffGID:])) | uint32(le.Uint16(in.raw[inodeOffGIDHigh:]))<<16
	atime = le.Uint32(in.raw[inodeOffATime:])
	mtime = le.Uint32(in.raw[inodeOffMTime:])
	ctime = le.Uint32(in.raw[inodeOffCTime:])
	mode = le.Uint16(in.raw[inodeOffMode:])
	return
}

func TestMetadataSetter(t *testing.T) {
	fs, cleanup := NewTempFS(t)
	defer cleanup()

	var _ filesystem.MetadataSetter = fs // capability is exposed

	if err := fs.WriteFile("/f", []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	before := uint32(time.Now().Unix()) - 1

	// Chmod: replace perm bits, keep the regular-file type bits, set setuid.
	if err := fs.Chmod("/f", 0o600|os.ModeSetuid); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	_, _, _, _, _, mode := readInodeFields(t, fs, "/f")
	if mode&0xF000 != 0x8000 {
		t.Fatalf("Chmod clobbered type bits: mode=0o%o", mode)
	}
	if mode&0o7777 != 0o4600 {
		t.Fatalf("Chmod perm = 0o%o, want 0o4600", mode&0o7777)
	}

	// Chown: 32-bit uid/gid split across low/high halves.
	const wantUID, wantGID = uint32(0x12345), uint32(0x6789A)
	if err := fs.Chown("/f", wantUID, wantGID); err != nil {
		t.Fatalf("Chown: %v", err)
	}
	uid, gid, _, _, _, mode2 := readInodeFields(t, fs, "/f")
	if uid != wantUID || gid != wantGID {
		t.Fatalf("Chown = uid %#x gid %#x, want %#x/%#x", uid, gid, wantUID, wantGID)
	}
	if mode2&0o7777 != 0o4600 {
		t.Fatalf("Chown changed mode: 0o%o", mode2&0o7777)
	}

	// Chtimes: explicit atime/mtime, ctime refreshed to ~now.
	at := time.Unix(1_000_000_000, 0)
	mt := time.Unix(1_500_000_000, 0)
	if err := fs.Chtimes("/f", at, mt); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	_, _, atime, mtime, ctime, _ := readInodeFields(t, fs, "/f")
	if atime != uint32(at.Unix()) || mtime != uint32(mt.Unix()) {
		t.Fatalf("Chtimes = atime %d mtime %d, want %d/%d", atime, mtime, at.Unix(), mt.Unix())
	}
	if ctime < before {
		t.Fatalf("ctime %d not refreshed (>= %d expected)", ctime, before)
	}
}
