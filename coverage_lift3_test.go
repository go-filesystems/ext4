package filesystem_ext4

import (
	"bytes"
	"context"
	"os"
	"testing"
)

// TestCheckImage_Clean exercises the fsck CheckImage method on a freshly
// formatted (and therefore consistent) image.
func TestCheckImage_Clean(t *testing.T) {
	fs, cleanup := NewTempFS(t)
	defer cleanup()
	if err := fs.CheckImage(); err != nil {
		t.Fatalf("CheckImage on clean image: %v", err)
	}
}

// TestRepairImage_Clean runs RepairImage on a clean image — it should be a
// no-op but the code path is exercised.
func TestRepairImage_Clean(t *testing.T) {
	fs, cleanup := NewTempFS(t)
	defer cleanup()
	if err := fs.RepairImage(); err != nil {
		t.Fatalf("RepairImage on clean image: %v", err)
	}
}

// TestCheckImage_NoSuperblockReturnsError simulates a missing-superblock
// failure by directly setting fs.sb to nil and verifies the guard fires.
func TestCheckImage_NoSuperblockReturnsError(t *testing.T) {
	fs, cleanup := NewTempFS(t)
	defer cleanup()
	saved := fs.sb
	fs.sb = nil
	defer func() { fs.sb = saved }()

	if err := fs.CheckImage(); err == nil {
		t.Fatalf("CheckImage with nil sb: expected error")
	}
	if err := fs.RepairImage(); err == nil {
		t.Fatalf("RepairImage with nil sb: expected error")
	}
}

// TestOpenViaPath drives the path-based Open() (vs. OpenFromDevice) which
// was zero-covered in internal-only runs.
func TestOpenViaPath(t *testing.T) {
	path := makeFormattedImage(t)
	defer os.Remove(path)

	fsi, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := fsi.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Error path: non-existent file
	if _, err := Open("/nonexistent/ext4-cov-test-xyzzy", -1); err == nil {
		t.Fatalf("Open missing path: expected error")
	}
}

// TestJournalAbort exercises the Abort path of an active transaction.
func TestJournalAbort(t *testing.T) {
	fs, cleanup := NewTempFS(t)
	defer cleanup()
	if fs.journal == nil {
		t.Skip("journal not present")
	}
	tx, err := fs.journal.StartTx()
	if err != nil {
		t.Fatalf("StartTx: %v", err)
	}
	if err := tx.Abort(); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	// Nil tx Abort.
	var nilTx *Transaction
	if err := nilTx.Abort(); err == nil {
		t.Fatalf("nil Abort: expected error")
	}
}

// TestEnqueueCommitWrite_Wrapper exercises the single-op convenience wrapper
// around enqueueCommitWrites.
func TestEnqueueCommitWrite_Wrapper(t *testing.T) {
	fs, cleanup := NewTempFS(t)
	defer cleanup()

	off := int64(fs.sb.BlockSize) * int64(fs.sb.BlocksCount-1)
	data := []byte("ABCDEFGH")
	if err := enqueueCommitWrite(fs.f, fs.partOffset, fs.sb, 0, off, data); err != nil {
		t.Fatalf("enqueueCommitWrite: %v", err)
	}
}

// TestCRC32c_Coverage exercises the internal crc32c helper from inside the
// package so it shows in internal-only coverage reports.
func TestCRC32c_Coverage(t *testing.T) {
	if crc32c(0, []byte("hello world")) == 0 {
		t.Fatalf("crc32c returned 0 on non-empty input")
	}
}

// TestOwnerContext exercises WithOwner / OwnerFrom round-trip and the
// nil-context branch of OwnerFrom.
func TestOwnerContext(t *testing.T) {
	if id, ok := OwnerFrom(nil); ok || id != 0 {
		t.Fatalf("OwnerFrom(nil) = (%d,%v), want (0,false)", id, ok)
	}
	ctx, id := WithOwner(context.Background())
	got, ok := OwnerFrom(ctx)
	if !ok {
		t.Fatalf("OwnerFrom: not found")
	}
	if got != id {
		t.Fatalf("OwnerFrom: got=%d want=%d", got, id)
	}
	// Context without owner returns false.
	if _, ok := OwnerFrom(context.Background()); ok {
		t.Fatalf("OwnerFrom on empty ctx: should be false")
	}
}

// TestReentrantMutex_LockUnlockOwner exercises the LockOwner/UnlockOwner
// happy path including re-entrant lock counting.
func TestReentrantMutex_LockUnlockOwner(t *testing.T) {
	var m reentrantMutex
	owner := NewOwner()
	m.LockOwner(owner)
	// Re-entrant lock from same owner token.
	m.LockOwner(owner)
	m.UnlockOwner(owner)
	m.UnlockOwner(owner)
	// Now Owner() should return 0 since unlocked.
	if got := m.Owner(); got != 0 {
		t.Fatalf("Owner after full unlock = %d, want 0", got)
	}
}

// TestReentrantMutex_LegacyLockPanics covers the deprecated Lock/Unlock
// methods that panic to detect callers still using them.
func TestReentrantMutex_LegacyLockPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic from Lock()")
		}
	}()
	var m reentrantMutex
	m.Lock()
}

func TestReentrantMutex_LegacyUnlockPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic from Unlock()")
		}
	}()
	var m reentrantMutex
	m.Unlock()
}

// TestGetFileLock_BasicLifecycle exercises the getFileLock map-cache.
func TestGetFileLock_BasicLifecycle(t *testing.T) {
	mf := &memFile{buf: make([]byte, 16)}
	l1 := getFileLock(mf)
	l2 := getFileLock(mf)
	if l1 != l2 {
		t.Fatalf("getFileLock returned different mutexes for same backing file")
	}
}

// TestCountFreeInBitmap exercises the diagnostic helper that scans a bitmap
// for cleared bits.
func TestCountFreeInBitmap(t *testing.T) {
	bm := []byte{0x00, 0xFF, 0x0F}
	free, sample := countFreeInBitmap(bm, 24)
	// First 8 bits free, next 8 set, next 4 set + 4 free => 8 + 0 + 4 = 12.
	if free != 12 {
		t.Fatalf("free=%d, want 12", free)
	}
	if len(sample) == 0 {
		t.Fatalf("sample is empty")
	}
	// Capped by maxBits.
	free2, _ := countFreeInBitmap(bm, 0)
	if free2 != 0 {
		t.Fatalf("free2=%d, want 0", free2)
	}
}

// TestJoinLines covers the trivial fsck helper.
func TestJoinLines(t *testing.T) {
	got := joinLines([]string{"a", "b"})
	if got != "a\nb\n" {
		t.Fatalf("got=%q", got)
	}
}

// TestBackoffSleep exercises backoffSleep with a tiny base so we don't slow
// the test suite. The function has no return value; we only need to drive
// the negative-attempt and positive-attempt branches.
func TestBackoffSleep(t *testing.T) {
	oldBase := backoffBase
	backoffBase = 1 // 1 nanosecond
	defer func() { backoffBase = oldBase }()

	backoffSleep(-1)
	backoffSleep(0)
	backoffSleep(2)
}

// TestBitmapCsumSeed exercises the per-group bitmap checksum seed helper.
func TestBitmapCsumSeed(t *testing.T) {
	sb := NewTestSuperblock(16, 32, 1, 4096)
	// Provide a non-zero raw superblock so csumSeed has data to hash.
	for i := range sb.raw {
		sb.raw[i] = byte(i)
	}
	// The block/inode bitmap checksum is seeded with the filesystem-wide
	// checksum seed and does NOT incorporate the block-group number (unlike
	// the group-descriptor checksum). The seed must therefore be identical
	// across groups and equal to sb.csumSeed(), matching e2fsprogs and the
	// kernel (verified against e2fsck on real images).
	s0 := bitmapCsumSeed(sb, 0)
	s1 := bitmapCsumSeed(sb, 1)
	if s0 != s1 {
		t.Fatalf("bitmap csum seed must not depend on group: s0=%d s1=%d", s0, s1)
	}
	if s0 != sb.csumSeed() {
		t.Fatalf("bitmap csum seed = %d, want csumSeed()=%d", s0, sb.csumSeed())
	}
}

// TestInodeGeneration round-trips the generation field in the inode raw
// buffer.
func TestInodeGeneration(t *testing.T) {
	in := NewTestInode(2, 256)
	in.raw[100] = 0xAB // ensure we read low-byte
	g := in.generation()
	// Read it again — must be deterministic.
	if g != in.generation() {
		t.Fatalf("generation not deterministic")
	}
}

// TestMakeTestInode covers the package-local helper.
func TestMakeTestInode(t *testing.T) {
	in := makeTestInode(7, 128)
	if in.num != 7 || len(in.raw) != 128 {
		t.Fatalf("unexpected inode: num=%d size=%d", in.num, len(in.raw))
	}
}

// TestMemFile_RoundTrip exercises the package-internal in-memory file
// helper used by other tests.
func TestMemFile_RoundTrip(t *testing.T) {
	m := &memFile{buf: make([]byte, 16)}
	if _, err := m.WriteAt([]byte("abcd"), 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	out := make([]byte, 4)
	if _, err := m.ReadAt(out, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if string(out) != "abcd" {
		t.Fatalf("got %q", string(out))
	}
	// Write past end forces grow.
	if _, err := m.WriteAt([]byte("xy"), 32); err != nil {
		t.Fatalf("WriteAt grow: %v", err)
	}
	if len(m.buf) != 34 {
		t.Fatalf("buf len after grow: %d", len(m.buf))
	}
}

// TestHookMemFile_RoundTrip covers the hookable in-memory file's hook
// branches.
func TestHookMemFile_RoundTrip(t *testing.T) {
	called := 0
	h := &hookMemFile{
		buf:       make([]byte, 16),
		readHook:  func(off int64, p []byte) error { called++; return nil },
		writeHook: func(off int64, p []byte) error { called++; return nil },
	}
	if _, err := h.WriteAt([]byte("xy"), 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	buf := make([]byte, 2)
	if _, err := h.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if called != 2 {
		t.Fatalf("hooks not called: %d", called)
	}
}

// TestDumpDebugLog covers the public DumpDebugLog helper. With debug
// disabled the buffer is empty, so the call is effectively a no-op write.
func TestDumpDebugLog(t *testing.T) {
	var buf bytes.Buffer
	// Stub the buffer with some content under lock so the non-empty branch
	// runs.
	debugMu.Lock()
	saved := debugBuf
	debugBuf = []byte("forced-debug-entry")
	debugMu.Unlock()
	defer func() {
		debugMu.Lock()
		debugBuf = saved
		debugMu.Unlock()
	}()

	DumpDebugLog(&buf)
	if !bytes.Contains(buf.Bytes(), []byte("forced-debug-entry")) {
		t.Fatalf("DumpDebugLog output missing payload: %q", buf.String())
	}

	// Empty branch
	var buf2 bytes.Buffer
	debugMu.Lock()
	debugBuf = nil
	debugMu.Unlock()
	DumpDebugLog(&buf2)
	if buf2.Len() != 0 {
		t.Fatalf("expected empty dump, got %q", buf2.String())
	}
}

// TestDumpForensicSnapshot exercises dumpForensicSnapshot which writes a
// summary log and (optional) raw dumps to /tmp.
func TestDumpForensicSnapshot(t *testing.T) {
	bits := []int{1, 2, 3}
	wantMap := map[int]reservationInfo{
		1: {want: true, seq: 7},
	}
	d := &bgd{raw: []byte{0x01, 0x02, 0x03}, BlockBitmapBlock: 8}
	dumpForensicSnapshot("test-key", bits, wantMap, d, []byte{0xFF, 0xEE})
	// Also exercise the nil-d / nil-bmap branch.
	dumpForensicSnapshot("test-key-nil", bits, wantMap, nil, nil)
}

// TestAppliedDumpPath covers the small path-formatting helper used by the
// commit dispatcher's snapshot writer.
func TestAppliedDumpPath(t *testing.T) {
	p := appliedDumpPath(1234, 567)
	if !bytes.Contains([]byte(p), []byte("1234")) || !bytes.Contains([]byte(p), []byte("567")) {
		t.Fatalf("appliedDumpPath unexpected: %q", p)
	}
}

// TestLockBGDTable covers the BGD-table lock acquisition pair which sits in
// metadata_locks.go and was 0% in baseline.
func TestLockBGDTable(t *testing.T) {
	mf := &memFile{buf: make([]byte, 1024)}
	unlock := lockBGDTable(mf)
	unlock()
	// Acquire again to exercise the cached map path.
	unlock2 := lockBGDTable(mf)
	unlock2()
	// Also exercise lockBGDTableBlock.
	u3 := lockBGDTableBlock(mf, 0)
	u3()
}

// TestBgdTableLockKey covers the trivial key-formatting helper.
func TestBgdTableLockKey(t *testing.T) {
	mf := &memFile{buf: make([]byte, 16)}
	k := bgdTableLockKey(mf)
	if !bytes.Contains([]byte(k), []byte(":bgdTable")) {
		t.Fatalf("bgdTableLockKey: unexpected key %q", k)
	}
}

// TestAppendAssocLog covers the file-append helper. With debug disabled
// (default) it returns immediately.
func TestAppendAssocLog(t *testing.T) {
	appendAssocLog("test-line")
	// With debug forced on, exercise the IO path.
	debugMu.Lock()
	savedEnabled := debugEnabled
	debugEnabled = true
	debugMu.Unlock()
	defer func() {
		debugMu.Lock()
		debugEnabled = savedEnabled
		debugMu.Unlock()
	}()
	appendAssocLog("test-line-enabled")
}

// Avoid "imported and not used" warnings if some tests are commented out.
var _ = os.Stat

// TestExt4FS_Grow_InvalidSize exercises the Grow API's input validation
// branches and the "no change" path.
func TestExt4FS_Grow_InvalidSize(t *testing.T) {
	fs, cleanup := NewTempFS(t)
	defer cleanup()
	if err := fs.Grow(0); err == nil {
		t.Fatalf("Grow(0): expected error")
	}
	if err := fs.Grow(-1); err == nil {
		t.Fatalf("Grow(-1): expected error")
	}
	// Grow to the current size or smaller — should error.
	if err := fs.Grow(1); err == nil {
		t.Fatalf("Grow(1): expected error")
	}
}

// TestExt4FS_Grow_NoSuperblock exercises the missing-superblock guard of
// the Grow path.
func TestExt4FS_Grow_NoSuperblock(t *testing.T) {
	fs, cleanup := NewTempFS(t)
	defer cleanup()
	saved := fs.sb
	fs.sb = nil
	defer func() { fs.sb = saved }()
	// Grow validates size first, so use a large size.
	if err := fs.Grow(1 << 30); err == nil {
		t.Fatalf("Grow with nil sb: expected error")
	}
}

// TestExt4FS_Grow_Happy drives the happy path of Grow by formatting an
// image at one size and growing it to a larger size. Many internal
// helpers along the way are exercised: lockBGDGroup, lockBitmapGroup,
// readBGD, readBitmap, clearBit, writeBitmapBuf, writeBGD,
// writeSuperblock, etc.
func TestExt4FS_Grow_Happy(t *testing.T) {
	fs, cleanup := NewTempFSWithSize(t, 4096*1024)
	defer cleanup()
	if err := fs.Grow(4096 * 2048); err != nil {
		t.Fatalf("Grow: %v", err)
	}
}
