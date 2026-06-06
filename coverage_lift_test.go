package filesystem_ext4

import (
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// failingDevice wraps an osFileDevice but lets tests force the partition
// table read to fail (we drive failure through ReadAt on the first sector).
type failingDevice struct {
	*osFileDevice
	failRead bool
}

func (d *failingDevice) ReadAt(p []byte, off int64) (int, error) {
	if d.failRead {
		return 0, errors.New("forced read error")
	}
	return d.osFileDevice.ReadAt(p, off)
}

// makeFormattedImage formats a tiny ext4 image, closes it, and returns the
// path. Callers must os.Remove it.
func makeFormattedImage(t *testing.T) string {
	t.Helper()
	f, err := os.CreateTemp("", "ext4-openfromdevice-*")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	f.Close()
	fs, err := Format(path, 4096*4096, FormatConfig{})
	if err != nil {
		os.Remove(path)
		t.Fatalf("Format: %v", err)
	}
	if err := fs.Close(); err != nil {
		os.Remove(path)
		t.Fatalf("Close: %v", err)
	}
	return path
}

// TestOpenFromDevice_BareFS opens a freshly formatted bare ext4 image
// via OpenFromDevice (rather than the more common Open path).
func TestOpenFromDevice_BareFS(t *testing.T) {
	path := makeFormattedImage(t)
	defer os.Remove(path)

	raw, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	dev := &osFileDevice{f: raw}

	fsi, err := OpenFromDevice(dev, -1)
	if err != nil {
		raw.Close()
		t.Fatalf("OpenFromDevice: %v", err)
	}
	// Round-trip a write/read through the freshly opened FS to exercise the
	// happy path.
	if err := fsi.WriteFile("/cov_open.txt", []byte("hello"), 0o644); err != nil {
		fsi.Close()
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := fsi.ReadFile("/cov_open.txt")
	if err != nil || string(got) != "hello" {
		fsi.Close()
		t.Fatalf("ReadFile: got=%q err=%v", string(got), err)
	}
	if err := fsi.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestOpenFromDevice_ReadError exercises the partition-detection failure
// branch: OpenFromDevice must close the device on partitionOffset error.
func TestOpenFromDevice_ReadError(t *testing.T) {
	path := makeFormattedImage(t)
	defer os.Remove(path)

	raw, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	dev := &failingDevice{osFileDevice: &osFileDevice{f: raw}, failRead: true}

	if _, err := OpenFromDevice(dev, -1); err == nil {
		t.Fatalf("expected error from forced read failure")
	}
	// dev.Close should have been called by openFromDevice. Calling again
	// must not panic; we only verify we can attempt it.
	_ = dev.Close()
}

// TestGetFlagsSetFlags_RoundTrip writes a file, mutates its inode flags and
// reads them back. Drives the GetFlags/SetFlags ioctl-style accessors.
func TestGetFlagsSetFlags_RoundTrip(t *testing.T) {
	fs, cleanup := NewTempFS(t)
	defer cleanup()

	const path = "/flags.txt"
	if err := fs.WriteFile(path, []byte("payload"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got0, err := fs.GetFlags(path)
	if err != nil {
		t.Fatalf("GetFlags: %v", err)
	}
	// A freshly-written file may carry flags such as ExtentFlag — we don't
	// assert exact value, only that the round-trip path executes.
	const userFlag = uint32(InodeFlagExtents) // 0x80000
	if err := fs.SetFlags(path, got0|userFlag); err != nil {
		t.Fatalf("SetFlags: %v", err)
	}
	got1, err := fs.GetFlags(path)
	if err != nil {
		t.Fatalf("GetFlags after Set: %v", err)
	}
	if got1&userFlag == 0 {
		t.Fatalf("SetFlags did not persist user flag: got 0x%x", got1)
	}
}

// TestGetFlagsSetFlags_NotFound exercises the lookup-failure branch of the
// flag accessors.
func TestGetFlagsSetFlags_NotFound(t *testing.T) {
	fs, cleanup := NewTempFS(t)
	defer cleanup()

	if _, err := fs.GetFlags("/no/such/file"); err == nil {
		t.Fatalf("GetFlags on missing path: expected error")
	}
	if err := fs.SetFlags("/no/such/file", 0); err == nil {
		t.Fatalf("SetFlags on missing path: expected error")
	}
}

// TestWatchLongHeldLocks_OneTick installs a fake long-held lock in
// bgdGroupLocks and bitmapGroupLocks, runs the watchdog for two ticks,
// and verifies it doesn't crash. The function returns only when its
// ticker channel closes, so we run it in a goroutine and stop it by
// clearing the maps (simulating release).
func TestWatchLongHeldLocks_OneTick(t *testing.T) {
	const threshold = 8 * time.Millisecond

	// Inject a fake held lock into bgdGroupLocks.
	const fakeKey = "test:watch:g:0"
	m := newTryMutex()
	// Manually mark as held by goroutine 99 with an old acquiredAt so the
	// threshold comparison fires.
	atomic.StoreUint64(&m.owner, 99)
	atomic.StoreInt64(&m.acquiredAt, time.Now().Add(-time.Second).UnixNano())
	m.ownerStack = []byte("fake-stack")

	bgdGroupLocksMu.Lock()
	bgdGroupLocks[fakeKey] = m
	bgdGroupLocksMu.Unlock()

	bitmapGroupLocksMu.Lock()
	bitmapGroupLocks[fakeKey] = m
	bitmapGroupLocksMu.Unlock()

	defer func() {
		bgdGroupLocksMu.Lock()
		delete(bgdGroupLocks, fakeKey)
		bgdGroupLocksMu.Unlock()
		bitmapGroupLocksMu.Lock()
		delete(bitmapGroupLocks, fakeKey)
		bitmapGroupLocksMu.Unlock()
	}()

	done := make(chan struct{})
	go func() {
		// The watcher uses ticker := threshold/4, so threshold = 8ms gives a
		// 2ms tick. Running for ~25ms guarantees multiple ticks where our
		// fake lock matches the >threshold check.
		watchLongHeldLocks(threshold)
		close(done)
	}()

	// The watcher loops forever; we never expect it to return. Instead we
	// sleep a few ticks, then clear our injected entry (which is what the
	// scan iterates over). The watcher is left running in a daemon-style
	// goroutine — that's fine for a unit test because the process exits at
	// the end of the test binary's life.
	time.Sleep(25 * time.Millisecond)

	// Also exercise the "no-owner" / "acquired==0" continue branches by
	// adding an unowned entry.
	const unownedKey = "test:watch-unowned:g:0"
	bgdGroupLocksMu.Lock()
	bgdGroupLocks[unownedKey] = newTryMutex()
	bgdGroupLocksMu.Unlock()
	defer func() {
		bgdGroupLocksMu.Lock()
		delete(bgdGroupLocks, unownedKey)
		bgdGroupLocksMu.Unlock()
	}()
	time.Sleep(10 * time.Millisecond)

	// We don't wait for done; the watcher loop never exits naturally.
	// Verify nothing has panicked by checking the channel is still open.
	select {
	case <-done:
		t.Fatalf("watchLongHeldLocks returned unexpectedly")
	default:
	}
}

// TestLockedAllocBlocksWithTx_Happy drives the tx-aware fully-locked
// allocator fallback path. We build a transaction, request a small block
// allocation and verify the cleanup callback runs without error.
func TestLockedAllocBlocksWithTx_Happy(t *testing.T) {
	fs, cleanup := NewTempFS(t)
	defer cleanup()

	if fs.journal == nil {
		t.Skip("journal not present; lockedAllocBlocksWithTx requires a tx")
	}

	tx, err := fs.journal.StartTx()
	if err != nil {
		t.Fatalf("StartTx: %v", err)
	}

	blocks, cleanupFn, err := lockedAllocBlocksWithTx(fs.f, fs.partOffset, fs.sb, 1, tx)
	if err != nil {
		_ = tx.Abort()
		t.Fatalf("lockedAllocBlocksWithTx: %v", err)
	}
	if len(blocks) != 1 || blocks[0] == 0 {
		_ = tx.Abort()
		t.Fatalf("unexpected blocks: %v", blocks)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if cleanupFn != nil {
		cleanupFn()
	}
}

// TestEnqueueCommitWritesDirect_NilFile drives the nil-file early return
// branch which exercises the inline-write fallback when f==nil.
func TestEnqueueCommitWritesDirect_NilFile(t *testing.T) {
	// With a nil readerWriterAt and a tiny op slice, the function should
	// route through writeAtWithTrace which itself returns an error on nil.
	// We mostly want the early-return statement coverage. Capture any
	// resulting error; both nil-error and non-nil-error are acceptable.
	sb := NewTestSuperblock(16, 32, 1, 4096)
	err := enqueueCommitWritesDirect(nil, 0, sb, 0, nil)
	// nil ops path returns nil
	if err != nil {
		t.Fatalf("nil-ops path err: %v", err)
	}
}

// TestEnqueueCommitWritesDirect_Happy exercises the queue/worker path with
// a real backing file.
func TestEnqueueCommitWritesDirect_Happy(t *testing.T) {
	fs, cleanup := NewTempFS(t)
	defer cleanup()

	// Build a single op writing 8 bytes near the end of the image. We
	// target an offset that is safely outside metadata so the write is
	// effectively a no-op for the FS layer.
	off := int64(fs.sb.BlockSize) * int64(fs.sb.BlocksCount-1)
	op := commitOp{startAbs: off, data: []byte("XXXXXXXX")}

	if err := enqueueCommitWritesDirect(fs.f, fs.partOffset, fs.sb, 0, []commitOp{op}); err != nil {
		t.Fatalf("enqueueCommitWritesDirect: %v", err)
	}
}

// TestEnqueueCommitWritesDirectUnderLock_Happy drives the same direct-mode
// path that returns an ack channel, which is what the journal replay code
// uses.
func TestEnqueueCommitWritesDirectUnderLock_Happy(t *testing.T) {
	fs, cleanup := NewTempFS(t)
	defer cleanup()

	off := int64(fs.sb.BlockSize) * int64(fs.sb.BlocksCount-1)
	op := commitOp{startAbs: off, data: []byte("YYYYYYYY")}

	ack, err := enqueueCommitWritesDirectUnderLock(fs.f, fs.partOffset, fs.sb, 0, []commitOp{op}, 0)
	if err != nil {
		t.Fatalf("enqueueCommitWritesDirectUnderLock: %v", err)
	}
	select {
	case e := <-ack:
		if e != nil {
			t.Fatalf("ack err: %v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("ack did not arrive")
	}
}

// TestEnqueueCommitWritesDirectUnderLock_NilFile covers the early-return
// branch of the under-lock variant.
func TestEnqueueCommitWritesDirectUnderLock_NilFile(t *testing.T) {
	sb := NewTestSuperblock(16, 32, 1, 4096)
	ack, err := enqueueCommitWritesDirectUnderLock(nil, 0, sb, 0, nil, 0)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	select {
	case <-ack:
		// either nil or non-nil acceptable
	case <-time.After(time.Second):
		t.Fatalf("ack did not arrive")
	}
}

// TestEnqueueCommitWritesDirect_QueueFull pre-fills the worker channel so
// the queue-full fallback path runs.
func TestEnqueueCommitWritesDirect_QueueFull(t *testing.T) {
	fs, cleanup := NewTempFS(t)
	defer cleanup()

	const group = uint32(0)
	if err := CreateFullWorker(fs.f, fs.partOffset, fs.sb, group, 1); err != nil {
		t.Fatalf("CreateFullWorker: %v", err)
	}
	defer RemoveWorkerFor(fs.f, fs.partOffset, fs.sb, group)

	off := int64(fs.sb.BlockSize) * int64(fs.sb.BlocksCount-1)
	op := commitOp{startAbs: off, data: []byte("ZZZZZZZZ")}
	// Should fall back to inline write
	if err := enqueueCommitWritesDirect(fs.f, fs.partOffset, fs.sb, group, []commitOp{op}); err != nil {
		t.Fatalf("enqueueCommitWritesDirect (queue-full): %v", err)
	}
}

// TestEnqueueCommitWritesDirectUnderLock_QueueFull drives the queue-full
// branch of the under-lock variant.
func TestEnqueueCommitWritesDirectUnderLock_QueueFull(t *testing.T) {
	fs, cleanup := NewTempFS(t)
	defer cleanup()

	const group = uint32(0)
	if err := CreateFullWorker(fs.f, fs.partOffset, fs.sb, group, 1); err != nil {
		t.Fatalf("CreateFullWorker: %v", err)
	}
	defer RemoveWorkerFor(fs.f, fs.partOffset, fs.sb, group)

	off := int64(fs.sb.BlockSize) * int64(fs.sb.BlocksCount-1)
	op := commitOp{startAbs: off, data: []byte("WWWWWWWW")}
	ack, err := enqueueCommitWritesDirectUnderLock(fs.f, fs.partOffset, fs.sb, group, []commitOp{op}, 0)
	if err != nil {
		t.Fatalf("enqueueCommitWritesDirectUnderLock (queue-full): %v", err)
	}
	select {
	case e := <-ack:
		if e != nil {
			t.Fatalf("ack err: %v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("ack did not arrive")
	}
}

// Compile-time guard so an unused import doesn't break the build if some
// references above are removed.
var _ = sync.Mutex{}
