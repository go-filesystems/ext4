package filesystem_ext4

import (
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// TestExt4FS_HighLevelOps exercises the ext4FS methods that wire the public
// filesystem.Filesystem interface to the internal helpers: MkDir, ListDir,
// Stat, DeleteFile, DeleteDir, Rename, ReadLink, WriteFile, ReadFile. These
// are thin wrappers but had 0% coverage in internal-only profiles.
func TestExt4FS_HighLevelOps(t *testing.T) {
	// This exercise drives the full journalled write/delete/rename path and is
	// dominated by real-time journal commit grace periods (~18s of wall clock).
	// Under QEMU emulation that wall time stacks on top of CPU emulation
	// overhead, so skip it in -short mode (the emulated arch jobs run -short);
	// the fast LE-decoder tests still run there, which is what exercises the
	// big-endian (s390x) byte-order path.
	if testing.Short() {
		t.Skip("skipping slow journalled round-trip in -short mode")
	}
	fs, cleanup := NewTempFS(t)
	defer cleanup()

	// MkDir
	if err := fs.MkDir("/sub", 0o755); err != nil {
		t.Fatalf("MkDir: %v", err)
	}
	if err := fs.MkDir("/sub/nested", 0o755); err != nil {
		t.Fatalf("MkDir nested: %v", err)
	}

	// WriteFile then ReadFile
	const filePath = "/sub/file.txt"
	if err := fs.WriteFile(filePath, []byte("hello world"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := fs.ReadFile(filePath)
	if err != nil || string(got) != "hello world" {
		t.Fatalf("ReadFile: got=%q err=%v", string(got), err)
	}

	// ListDir
	entries, err := fs.ListDir("/sub")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatalf("ListDir: expected entries")
	}

	// Stat
	st, err := fs.Stat(filePath)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if st.Size() != uint64(len("hello world")) {
		t.Fatalf("Stat: size=%d, want %d", st.Size(), len("hello world"))
	}

	// Add a symlink and ReadLink it.
	AddFastSymlink(t, fs, "/sub", "link.txt", "file.txt")
	target, err := fs.ReadLink("/sub/link.txt")
	if err != nil {
		t.Fatalf("ReadLink: %v", err)
	}
	if target != "file.txt" {
		t.Fatalf("ReadLink: target=%q, want %q", target, "file.txt")
	}

	// Rename
	if err := fs.Rename("/sub/file.txt", "/sub/file2.txt"); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	// DeleteFile
	if err := fs.DeleteFile("/sub/file2.txt"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}

	// Clean up the link and nested dir, then DeleteDir.
	_ = fs.DeleteFile("/sub/link.txt")
	if err := fs.DeleteDir("/sub/nested"); err != nil {
		t.Fatalf("DeleteDir nested: %v", err)
	}
	if err := fs.DeleteDir("/sub"); err != nil {
		t.Fatalf("DeleteDir: %v", err)
	}
}

// TestExt4FS_StatErrors exercises the error branches of the high-level
// methods.
func TestExt4FS_StatErrors(t *testing.T) {
	fs, cleanup := NewTempFS(t)
	defer cleanup()
	if _, err := fs.Stat("/missing"); err == nil {
		t.Fatalf("Stat: expected error")
	}
	if _, err := fs.ListDir("/missing"); err == nil {
		t.Fatalf("ListDir: expected error")
	}
	if _, err := fs.ReadLink("/missing"); err == nil {
		t.Fatalf("ReadLink: expected error")
	}
	// DeleteFile/DeleteDir may or may not error on missing paths depending
	// on internal idempotency rules; just exercise the call.
	_ = fs.DeleteFile("/missing")
	_ = fs.DeleteDir("/missing")
	if err := fs.Rename("/missing", "/also-missing"); err == nil {
		t.Fatalf("Rename: expected error")
	}
}

// TestBlockDeviceTruncateSize exercises the osFileDevice Size and Truncate
// implementations which are thin wrappers around *os.File.
func TestBlockDeviceTruncateSize(t *testing.T) {
	f, err := os.CreateTemp("", "ext4-blockdev-*")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	defer os.Remove(path)
	defer f.Close()

	dev := &osFileDevice{f: f}
	if err := dev.Truncate(1024); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	sz, err := dev.Size()
	if err != nil {
		t.Fatalf("Size: %v", err)
	}
	if sz != 1024 {
		t.Fatalf("Size: %d, want 1024", sz)
	}
	if dev.UnderlyingFile() == nil {
		t.Fatalf("UnderlyingFile returned nil")
	}
	if dev.Name() != path {
		t.Fatalf("Name: %q, want %q", dev.Name(), path)
	}
}

// TestGetSeqForAck_NilAndKnown exercises both branches of getSeqForAck:
// a nil ack returns (0, false), and a registered ack returns the seq.
func TestGetSeqForAck_NilAndKnown(t *testing.T) {
	if seq, ok := getSeqForAck(nil); seq != 0 || ok {
		t.Fatalf("getSeqForAck(nil) = (%d,%v), want (0,false)", seq, ok)
	}
	ack := make(chan error, 1)
	registerAckSeq(ack, 42)
	defer func() {
		ackToSeqMu.Lock()
		delete(ackToSeq, ack)
		delete(ackToSeqTs, ack)
		ackToSeqMu.Unlock()
	}()
	if seq, ok := getSeqForAck(ack); !ok || seq != 42 {
		t.Fatalf("getSeqForAck(known) = (%d,%v), want (42,true)", seq, ok)
	}
}

// TestAttachSeqToReservations_Guards exercises the early-return guards
// and the BGD-read-failure path of attachSeqToReservations.
func TestAttachSeqToReservations_Guards(t *testing.T) {
	// seq==0 fast path
	attachSeqToReservations(nil, 0, nil, 0, 0, nil)
	// f==nil fast path
	attachSeqToReservations(nil, 0, NewTestSuperblock(16, 32, 1, 4096), 0, 5, nil)

	// Drive the happy path against a real (empty) FS so readBGD succeeds.
	fs, cleanup := NewTempFS(t)
	defer cleanup()
	attachSeqToReservations(fs.f, fs.partOffset, fs.sb, 0, 5, nil)
}

// TestCleanupStaleReservations_OneRun seeds reservedBits and reservedWant
// with a stale entry and verifies cleanupStaleReservations clears it on at
// least one tick. As with watchLongHeldLocks the function never returns;
// we run it briefly in a goroutine and verify the side-effect.
func TestCleanupStaleReservations_OneRun(t *testing.T) {
	const key = "test:cleanup-stale:g:0"
	const threshold = 8 * time.Millisecond

	reservedBitsMu.Lock()
	// initial bitmap with bit 7 set
	reservedBits[key] = []byte{0x80}
	reservedWant[key] = map[int]reservationInfo{
		7: {want: true, ts: time.Now().Add(-time.Second).UnixNano()},
	}
	reservedBitsMu.Unlock()

	defer func() {
		reservedBitsMu.Lock()
		delete(reservedBits, key)
		delete(reservedWant, key)
		reservedBitsMu.Unlock()
	}()

	go cleanupStaleReservations(threshold)

	// Wait up to 200ms for the goroutine to clear the stale bit.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		reservedBitsMu.Lock()
		wm, ok := reservedWant[key]
		_, hasBit := wm[7]
		reservedBitsMu.Unlock()
		if !ok || !hasBit {
			return // success
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("cleanupStaleReservations did not clear the stale reservation")
}

// TestMarkAndIsSeqAcked exercises the seqAcked accessor pair which is a
// trivial side-effect helper but still has zero coverage in pure-internal
// runs.
func TestMarkAndIsSeqAcked(t *testing.T) {
	const seq = uint64(91723)
	if isSeqAcked(seq) {
		t.Fatalf("seq pre-marked unexpectedly")
	}
	markSeqAcked(seq)
	if !isSeqAcked(seq) {
		t.Fatalf("seq not acked after markSeqAcked")
	}
	// markSeqAcked(0) is a no-op fast path
	markSeqAcked(0)
}

// TestTryMutex_BasicLockUnlock drives the tryMutex Lock/Unlock/TryLock
// fast paths which are not directly exercised by current tests because
// they are called from internal helpers only.
func TestTryMutex_BasicLockUnlock(t *testing.T) {
	m := newTryMutex()
	if !m.TryLock() {
		t.Fatalf("TryLock should succeed on fresh mutex")
	}
	// Re-entrant TryLock from same goroutine.
	if !m.TryLock() {
		t.Fatalf("reentrant TryLock should succeed")
	}
	// Recursion counter should now be 2.
	if got := atomic.LoadInt32(&m.recursion); got != 2 {
		t.Fatalf("recursion=%d, want 2", got)
	}
	m.Unlock()
	m.Unlock()
}

// TestTryMutex_Blocking exercises the blocking Lock path by holding the
// mutex in one goroutine and contending from another.
func TestTryMutex_Blocking(t *testing.T) {
	m := newTryMutex()
	m.Lock()
	done := make(chan struct{})
	go func() {
		m.Lock()
		m.Unlock()
		close(done)
	}()
	// Hold briefly so the other goroutine reaches the channel receive.
	time.Sleep(5 * time.Millisecond)
	m.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("contending Lock did not progress")
	}
}
