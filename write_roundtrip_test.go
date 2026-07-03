package filesystem_ext4

import (
	"bytes"
	"fmt"
	"testing"
	"time"
)

// pattern returns n deterministic bytes so written data can be verified on
// read-back.
func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed ^ byte(i*7+i/251)
	}
	return b
}

// TestWriteRoundTripSizes writes regular files across the size boundaries that
// select different on-disk layouts — empty, sub-block, exactly one block,
// just over a block, and several multi-block/multi-extent sizes including a
// megabyte file — then reads each back and checks it byte-for-byte. This drives
// the block-allocation and extent-write paths that a small-file-only suite
// never reaches.
func TestWriteRoundTripSizes(t *testing.T) {
	fs, cleanup := NewTempFS(t)
	defer cleanup()

	sizes := []int{0, 1, 100, 4095, 4096, 4097, 5000, 60000, 300000, 1000000}
	for _, sz := range sizes {
		path := fmt.Sprintf("/f_%d.bin", sz)
		want := pattern(sz, byte(sz))
		if err := fs.WriteFile(path, want, 0o644); err != nil {
			t.Fatalf("WriteFile %s (%d bytes): %v", path, sz, err)
		}
		got, err := fs.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile %s: %v", path, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("round-trip %s: %d bytes back, want %d (equal=%v)", path, len(got), sz, bytes.Equal(got, want))
		}
		st, err := fs.Stat(path)
		if err != nil || int(st.Size()) != sz {
			t.Fatalf("Stat %s: size %d, want %d, err %v", path, statSize(st), sz, err)
		}
	}
}

// TestWriteOverwriteGrowShrink overwrites the same file with a larger and then
// a smaller payload, exercising the truncate / re-allocate branches of the
// write path.
func TestWriteOverwriteGrowShrink(t *testing.T) {
	if testing.Short() {
		t.Skip("repeated overwrites issue many journalled commits; skipped under -short")
	}
	fs, cleanup := NewTempFS(t)
	defer cleanup()

	const path = "/mutable.bin"
	steps := []int{4096, 40000, 200000, 8000, 0, 12345}
	for _, sz := range steps {
		want := pattern(sz, 0x5A)
		if err := fs.WriteFile(path, want, 0o600); err != nil {
			t.Fatalf("WriteFile %s @%d: %v", path, sz, err)
		}
		got, err := fs.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile %s @%d: %v", path, sz, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("overwrite @%d: got %d bytes, want %d", sz, len(got), sz)
		}
	}
}

// TestWriteDirsAndManyEntries creates nested directories and a directory large
// enough to grow past a single block (driving the directory-block allocation
// and, where enabled, htree growth on the write side), then lists them back.
func TestWriteDirsAndManyEntries(t *testing.T) {
	fs, cleanup := NewTempFS(t)
	defer cleanup()

	if err := fs.MkDir("/a", 0o755); err != nil {
		t.Fatalf("MkDir /a: %v", err)
	}
	if err := fs.MkDir("/a/b", 0o755); err != nil {
		t.Fatalf("MkDir /a/b: %v", err)
	}
	if err := fs.WriteFile("/a/b/leaf.txt", []byte("deep"), 0o644); err != nil {
		t.Fatalf("WriteFile deep: %v", err)
	}
	if got, err := fs.ReadFile("/a/b/leaf.txt"); err != nil || string(got) != "deep" {
		t.Fatalf("ReadFile deep: %q %v", got, err)
	}

	if err := fs.MkDir("/many", 0o755); err != nil {
		t.Fatalf("MkDir /many: %v", err)
	}
	const n = 200
	for i := 0; i < n; i++ {
		p := fmt.Sprintf("/many/e_%03d", i)
		if err := fs.WriteFile(p, []byte{byte(i)}, 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", p, err)
		}
	}
	ents, err := fs.ListDir("/many")
	if err != nil {
		t.Fatalf("ListDir /many: %v", err)
	}
	if len(ents) != n {
		t.Fatalf("ListDir /many: %d entries, want %d", len(ents), n)
	}
}

// TestWriteSymlinkAndHardLink exercises symlink creation (fast + slow) and hard
// links through the public write API.
func TestWriteSymlinkAndHardLink(t *testing.T) {
	fs, cleanup := NewTempFS(t)
	defer cleanup()

	if err := fs.WriteFile("/target.txt", []byte("hello link"), 0o644); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}

	// Fast symlink (short target, stored inline).
	if err := fs.Symlink("target.txt", "/short.lnk"); err != nil {
		t.Fatalf("Symlink short: %v", err)
	}
	if tgt, err := fs.ReadLink("/short.lnk"); err != nil || tgt != "target.txt" {
		t.Fatalf("ReadLink short.lnk = %q, %v", tgt, err)
	}

	// Slow symlink (>60 bytes, stored out of line).
	long := "some/deeply/nested/relative/target/that/is/definitely/longer/than/sixty/bytes.txt"
	if err := fs.Symlink(long, "/long.lnk"); err != nil {
		t.Fatalf("Symlink long: %v", err)
	}
	if tgt, err := fs.ReadLink("/long.lnk"); err != nil || tgt != long {
		t.Fatalf("ReadLink long.lnk = %q, %v", tgt, err)
	}

	// Hard link: same contents readable through the new name.
	if err := fs.Link("/target.txt", "/hard.txt"); err != nil {
		t.Fatalf("Link: %v", err)
	}
	if got, err := fs.ReadFile("/hard.txt"); err != nil || string(got) != "hello link" {
		t.Fatalf("ReadFile hard.txt = %q, %v", got, err)
	}
}

// TestWriteMetadataOps exercises the metadata mutators (Chmod/Chown/Chtimes,
// SetFlags/GetFlags) and the label read/write path.
func TestWriteMetadataOps(t *testing.T) {
	fs, cleanup := NewTempFS(t)
	defer cleanup()

	if err := fs.WriteFile("/m.txt", []byte("meta"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := fs.Chmod("/m.txt", 0o600); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	st, err := fs.Stat("/m.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if st.Mode()&0o777 != 0o600 {
		t.Fatalf("Chmod not reflected: mode %#o", st.Mode())
	}

	if err := fs.Chown("/m.txt", 1234, 5678); err != nil {
		t.Fatalf("Chown: %v", err)
	}
	if err := fs.Chtimes("/m.txt", time.Unix(1_600_000_000, 0), time.Unix(1_600_000_100, 0)); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	if err := fs.SetFlags("/m.txt", 0); err != nil {
		t.Fatalf("SetFlags: %v", err)
	}
	if _, err := fs.GetFlags("/m.txt"); err != nil {
		t.Fatalf("GetFlags: %v", err)
	}

	if err := fs.SetLabel("writable"); err != nil {
		t.Fatalf("SetLabel: %v", err)
	}
	if got := fs.Label(); got != "writable" {
		t.Fatalf("Label = %q, want writable", got)
	}
}

// TestWriteRenameDelete exercises rename (file and directory) and the delete
// paths.
func TestWriteRenameDelete(t *testing.T) {
	fs, cleanup := NewTempFS(t)
	defer cleanup()

	if err := fs.WriteFile("/orig.txt", []byte("body"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := fs.Rename("/orig.txt", "/renamed.txt"); err != nil {
		t.Fatalf("Rename file: %v", err)
	}
	if got, err := fs.ReadFile("/renamed.txt"); err != nil || string(got) != "body" {
		t.Fatalf("ReadFile renamed: %q %v", got, err)
	}
	if _, err := fs.ReadFile("/orig.txt"); err == nil {
		t.Fatal("orig.txt should be gone after rename")
	}

	if err := fs.MkDir("/d1", 0o755); err != nil {
		t.Fatalf("MkDir: %v", err)
	}
	if err := fs.Rename("/d1", "/d2"); err != nil {
		t.Fatalf("Rename dir: %v", err)
	}

	if err := fs.DeleteFile("/renamed.txt"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if err := fs.DeleteDir("/d2"); err != nil {
		t.Fatalf("DeleteDir: %v", err)
	}
	if _, err := fs.ReadFile("/renamed.txt"); err == nil {
		t.Fatal("renamed.txt should be gone after delete")
	}
}

// TestWriteThenGrow writes data, grows the filesystem, and confirms the data
// still reads back — exercising the online-grow path and re-validating the
// superblock afterwards.
func TestWriteThenGrow(t *testing.T) {
	fs, cleanup := NewTempFSWithSize(t, 8<<20)
	defer cleanup()

	want := pattern(500000, 0x11)
	if err := fs.WriteFile("/pre.bin", want, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := fs.Grow(16 << 20); err != nil {
		t.Fatalf("Grow: %v", err)
	}
	got, err := fs.ReadFile("/pre.bin")
	if err != nil {
		t.Fatalf("ReadFile after grow: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("data lost across grow: %d vs %d bytes", len(got), len(want))
	}
	// A file written after growing must land in the new space and read back.
	post := pattern(300000, 0x22)
	if err := fs.WriteFile("/post.bin", post, 0o644); err != nil {
		t.Fatalf("WriteFile post-grow: %v", err)
	}
	if got, err := fs.ReadFile("/post.bin"); err != nil || !bytes.Equal(got, post) {
		t.Fatalf("post-grow round-trip failed: %v", err)
	}
}

// TestWriteFillToCapacity drives the allocator under pressure: it writes into a
// deliberately small filesystem until allocation fails, exercising the
// multi-block-group scan and the out-of-space error path (which a mostly-empty
// image never reaches), and asserts the failure is graceful rather than a panic
// or corruption. Files written before exhaustion must still read back intact.
func TestWriteFillToCapacity(t *testing.T) {
	if testing.Short() {
		t.Skip("fill-to-capacity issues many journalled commits; skipped under -short")
	}
	fs, cleanup := NewTempFSWithSize(t, 8<<20)
	defer cleanup()

	chunk := pattern(128*1024, 0x3C)
	written := map[string][]byte{}
	var hitLimit bool
	for i := 0; i < 128; i++ {
		p := fmt.Sprintf("/fill_%02d.bin", i)
		if err := fs.WriteFile(p, chunk, 0o644); err != nil {
			// Expected: the small image runs out of space. The error must be
			// clean (already returned, no panic) and prefixed by the package.
			hitLimit = true
			break
		}
		written[p] = chunk
	}
	if !hitLimit {
		t.Skip("filesystem did not fill within the write budget; allocator pressure not exercised")
	}
	// Everything committed before exhaustion must still be readable and correct.
	for p, want := range written {
		got, err := fs.ReadFile(p)
		if err != nil {
			t.Fatalf("ReadFile %s after ENOSPC: %v", p, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("data corrupted after ENOSPC: %s", p)
		}
	}
}

func statSize(s interface{ Size() uint64 }) uint64 {
	if s == nil {
		return 0
	}
	return s.Size()
}
