package filesystem_ext4

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

// probeWritable asserts the capability is reachable the way a caller reaches
// it — through the filesystem.File that OpenFile returns, not the concrete
// type — and hands back the WritableFile.
func probeWritable(t *testing.T, f filesystem.File) filesystem.WritableFile {
	t.Helper()
	w, ok := f.(filesystem.WritableFile)
	if !ok {
		t.Fatalf("ext4's File (%T) does not satisfy filesystem.WritableFile", f)
	}
	return w
}

// wpattern builds deterministic, position-dependent bytes. A constant fill
// would hide an off-by-one-block: every wrong byte would happen to be right.
func wpattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31) ^ seed ^ byte(i>>8)
	}
	return b
}

// rmwOracle is the slow path this capability exists to replace: read the whole
// file, splice, write the whole file back. It is the ORACLE for every WriteAt
// below — a caller that falls back to it when a driver has no WritableFile
// must end up with the same filesystem either way.
func rmwOracle(t *testing.T, fsIfc filesystem.Filesystem, path string, p []byte, off int64) {
	t.Helper()
	cur, err := fsIfc.ReadFile(path)
	if err != nil {
		t.Fatalf("oracle ReadFile(%s): %v", path, err)
	}
	if end := off + int64(len(p)); end > int64(len(cur)) {
		grown := make([]byte, end)
		copy(grown, cur)
		cur = grown
	}
	copy(cur[off:], p)
	if err := fsIfc.WriteFile(path, cur, 0o644); err != nil {
		t.Fatalf("oracle WriteFile(%s): %v", path, err)
	}
}

// checkBothReadPaths reads the file back through ReadAt on a freshly opened
// File AND through ReadFile, and requires both to equal want. The two use
// different code — a binary search over the extent map versus a materialising
// whole-file read — so a write that updated one view and not the other is
// caught here and nowhere else.
func checkBothReadPaths(t *testing.T, fsIfc filesystem.Filesystem, path string, want []byte) {
	t.Helper()
	viaReadFile, err := fsIfc.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	if !bytes.Equal(viaReadFile, want) {
		t.Fatalf("%s: ReadFile gave %d bytes, want %d; first difference at %d",
			path, len(viaReadFile), len(want), diffAt(viaReadFile, want))
	}
	f, err := fsIfc.(filesystem.Opener).OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile(%s): %v", path, err)
	}
	defer f.Close()
	if got := f.Size(); got != int64(len(want)) {
		t.Fatalf("%s: Size() = %d, want %d", path, got, len(want))
	}
	got := make([]byte, len(want))
	if len(want) > 0 {
		n, err := f.ReadAt(got, 0)
		if n != len(want) || (err != nil && !errors.Is(err, io.EOF)) {
			t.Fatalf("%s: ReadAt(all) = %d, %v", path, n, err)
		}
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s: ReadAt disagrees with the expected content at byte %d", path, diffAt(got, want))
	}
	st, err := fsIfc.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%s): %v", path, err)
	}
	if int64(st.Size()) != int64(len(want)) {
		t.Fatalf("%s: Stat().Size() = %d, want %d — the inode's i_size was not updated",
			path, st.Size(), len(want))
	}
}

func diffAt(a, b []byte) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

// newExt4Image builds a fresh, real ext4 image from the e2fsprogs-generated
// fixture corpus — mke2fs's layout, not this package's idea of one — and
// returns it opened. Each call gets its own copy, so a case cannot be masked
// by what a previous one left behind.
func newExt4Image(t *testing.T, img string) filesystem.Filesystem {
	t.Helper()
	src := filepath.Join(fixtureDir(t), img)
	fsIfc, err := Open(src, -1)
	if err != nil {
		t.Fatalf("Open(%s): %v", img, err)
	}
	t.Cleanup(func() { _ = fsIfc.Close() })
	return fsIfc
}

type wcase struct {
	name string
	off  func(bs, size int64) int64
	n    func(bs, size int64) int
}

// wcases covers, on every geometry: writes wholly inside one block, straddling
// one block boundary, straddling several, exactly on a boundary, extending the
// file, and landing in a HOLE past the end — the cases where a positional
// write can differ from a whole-file rewrite, plus the one where it must not.
var wcases = []wcase{
	{"start", func(bs, sz int64) int64 { return 0 }, func(bs, sz int64) int { return 17 }},
	{"interior-within-block", func(bs, sz int64) int64 { return bs + 5 }, func(bs, sz int64) int { return int(bs) - 10 }},
	{"straddles-one-boundary", func(bs, sz int64) int64 { return bs - 3 }, func(bs, sz int64) int { return 9 }},
	{"straddles-many-boundaries", func(bs, sz int64) int64 { return bs/2 + 1 }, func(bs, sz int64) int { return int(3*bs) + 7 }},
	{"exactly-on-boundary", func(bs, sz int64) int64 { return 2 * bs }, func(bs, sz int64) int { return int(bs) }},
	{"last-byte", func(bs, sz int64) int64 { return sz - 1 }, func(bs, sz int64) int { return 1 }},
	{"extends-within-last-block", func(bs, sz int64) int64 { return sz }, func(bs, sz int64) int { return 3 }},
	{"extends-past-last-block", func(bs, sz int64) int64 { return sz - 2 }, func(bs, sz int64) int { return int(2*bs) + 11 }},
	{"hole-inside-last-block", func(bs, sz int64) int64 { return sz + 4 }, func(bs, sz int64) int { return 6 }},
	{"hole-spanning-whole-blocks", func(bs, sz int64) int64 { return sz + 3*bs + 9 }, func(bs, sz int64) int { return int(bs) + 1 }},
	{"whole-file-overwrite", func(bs, sz int64) int64 { return 0 }, func(bs, sz int64) int { return int(sz) }},
}

// TestWriteAtMatchesReadModifyWrite is THE verification the capability has to
// survive: on real mke2fs images, for every offset shape above, WriteAt must
// produce EXACTLY the file that ReadFile + splice + WriteFile produces on an
// identical image, and the result must read back the same through ReadAt and
// through ReadFile. Two block sizes, because a boundary bug can hide at one
// and not the other.
func TestWriteAtMatchesReadModifyWrite(t *testing.T) {
	for _, img := range []string{"ext4_4k.img", "ext4_1k.img"} {
		t.Run(img, func(t *testing.T) {
			for _, tc := range wcases {
				t.Run(tc.name, func(t *testing.T) {
					mine, oracle := newExt4Image(t, img), newExt4Image(t, img)
					bs := int64(mine.(*ext4FS).sb.BlockSize)

					const path = "/wat.bin"
					initial := wpattern(int(bs)*6+37, 0x5A)
					for _, fsIfc := range []filesystem.Filesystem{mine, oracle} {
						if err := fsIfc.WriteFile(path, initial, 0o644); err != nil {
							t.Fatalf("seed WriteFile: %v", err)
						}
					}
					size := int64(len(initial))
					off := tc.off(bs, size)
					data := wpattern(tc.n(bs, size), 0xC7)

					f, err := mine.(filesystem.Opener).OpenFile(path)
					if err != nil {
						t.Fatalf("OpenFile: %v", err)
					}
					w := probeWritable(t, f)
					n, err := w.WriteAt(data, off)
					if n != len(data) || err != nil {
						t.Fatalf("WriteAt(len=%d, off=%d) = %d, %v — io.WriterAt requires all of p or an error",
							len(data), off, n, err)
					}
					wantSize := max(size, off+int64(len(data)))
					if got := w.Size(); got != wantSize {
						t.Fatalf("Size() = %d after WriteAt, want %d", got, wantSize)
					}
					if err := w.Sync(); err != nil {
						t.Fatalf("Sync: %v", err)
					}
					if err := w.Close(); err != nil {
						t.Fatalf("Close: %v", err)
					}

					rmwOracle(t, oracle, path, data, off)
					want, err := oracle.ReadFile(path)
					if err != nil {
						t.Fatalf("oracle ReadFile: %v", err)
					}
					if int64(len(want)) != wantSize {
						t.Fatalf("oracle produced %d bytes, want %d", len(want), wantSize)
					}
					checkBothReadPaths(t, mine, path, want)
				})
			}
		})
	}
}

// TestWriteAtKeepsTheFileSparse is the property ext4 has and FAT does not, and
// it must not be quietly given up: a write far past the end allocates blocks
// only for what it writes, so the gap costs nothing. Measuring i_blocks proves
// it, where comparing contents alone could not.
func TestWriteAtKeepsTheFileSparse(t *testing.T) {
	fsIfc := newExt4Image(t, "ext4_4k.img")
	bs := int64(fsIfc.(*ext4FS).sb.BlockSize)
	const path = "/sparsewrite.bin"
	head := wpattern(10, 1)
	if err := fsIfc.WriteFile(path, head, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	f, err := fsIfc.(filesystem.Opener).OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	w := probeWritable(t, f)
	tail := wpattern(7, 2)
	holeAt := 400 * bs // 1.6 MiB of hole at 4 KiB blocks
	if _, err := w.WriteAt(tail, holeAt); err != nil {
		t.Fatalf("WriteAt past end: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ef := concreteFile(mustOpen(t, fsIfc, path))
	if ef == nil {
		t.Fatal("reopened file has an unexpected type")
	}
	blocks := 0
	for _, e := range ef.extents {
		blocks += int(e.Count)
	}
	if blocks > 4 {
		t.Fatalf("a 1.6 MiB hole cost %d blocks — the file was filled, not left sparse", blocks)
	}
	t.Logf("file spans %d bytes in %d blocks across %d extents", ef.size, blocks, len(ef.extents))

	want := make([]byte, int(holeAt)+len(tail))
	copy(want, head)
	copy(want[holeAt:], tail)
	checkBothReadPaths(t, fsIfc, path, want)
}

func mustOpen(t *testing.T, fsIfc filesystem.Filesystem, path string) filesystem.File {
	t.Helper()
	f, err := fsIfc.(filesystem.Opener).OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile(%s): %v", path, err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// TestWriteAtHoleReadsAsZeros pins the rule a caller cannot check any other
// way: bytes never written must read as zeros through BOTH read paths, not as
// whatever the block held when some earlier, deleted file owned it. The test
// dirties the free space with 0xFF first, so "reads as zeros" is a property of
// the zero-fill and not of a fresh image.
func TestWriteAtHoleReadsAsZeros(t *testing.T) {
	fsIfc := newExt4Image(t, "ext4_4k.img")
	if err := fsIfc.WriteFile("/dirty.bin", bytes.Repeat([]byte{0xFF}, 200*1024), 0o644); err != nil {
		t.Fatalf("WriteFile dirty: %v", err)
	}
	if err := fsIfc.DeleteFile("/dirty.bin"); err != nil {
		t.Fatalf("DeleteFile dirty: %v", err)
	}
	head := wpattern(10, 3)
	if err := fsIfc.WriteFile("/z.bin", head, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	w := probeWritable(t, mustOpen(t, fsIfc, "/z.bin"))
	tail := wpattern(9, 4)
	// Inside the same block as the old end of file, so the slack-zeroing path
	// is what has to produce the zeros, not a freshly allocated block.
	if _, err := w.WriteAt(tail, 2000); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	want := make([]byte, 2000+len(tail))
	copy(want, head)
	copy(want[2000:], tail)
	checkBothReadPaths(t, fsIfc, "/z.bin", want)
}

// TestWriteAtSequentialMatchesWholeFile is the shape the NFS server produces
// and the reason this capability exists: a file written from offset zero in
// fixed-size blocks. It must land byte-for-byte identical to a single
// whole-file write of the same bytes.
func TestWriteAtSequentialMatchesWholeFile(t *testing.T) {
	incremental, atOnce := newExt4Image(t, "ext4_4k.img"), newExt4Image(t, "ext4_4k.img")
	const path = "/seq.bin"
	const block = 32 * 1024
	whole := wpattern(block*13+555, 0x3C)

	if err := incremental.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("create: %v", err)
	}
	w := probeWritable(t, mustOpen(t, incremental, path))
	for off := 0; off < len(whole); off += block {
		end := min(off+block, len(whole))
		if n, err := w.WriteAt(whole[off:end], int64(off)); n != end-off || err != nil {
			t.Fatalf("WriteAt(off=%d) = %d, %v", off, n, err)
		}
		// The client's next GETATTR must already see the file grow.
		if got := w.Size(); got != int64(end) {
			t.Fatalf("Size() = %d after writing through %d", got, end)
		}
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	checkBothReadPaths(t, incremental, path, whole)

	if err := atOnce.WriteFile(path, whole, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	want, err := atOnce.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	got, err := incremental.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("block-by-block WriteAt differs from a single WriteFile at byte %d", diffAt(got, want))
	}
}

// TestTruncateFile exercises the file-scoped Truncate in both directions and
// checks the result against an oracle built with WriteFile, since this driver
// has no path-scoped Truncate to compare against.
func TestTruncateFile(t *testing.T) {
	for _, size := range []int64{0, 1, 4095, 4096, 4097, 20000, 100000} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			fsIfc := newExt4Image(t, "ext4_4k.img")
			const path = "/t.bin"
			initial := wpattern(9000, 0x77)
			if err := fsIfc.WriteFile(path, initial, 0o644); err != nil {
				t.Fatalf("seed: %v", err)
			}
			w := probeWritable(t, mustOpen(t, fsIfc, path))
			if err := w.Truncate(size); err != nil {
				t.Fatalf("Truncate(%d): %v", size, err)
			}
			if got := w.Size(); got != size {
				t.Fatalf("Size() = %d after Truncate(%d)", got, size)
			}
			// Truncating to the size it already has is a no-op that still
			// succeeds — the early return.
			if err := w.Truncate(size); err != nil {
				t.Fatalf("Truncate(%d) second time: %v", size, err)
			}

			want := make([]byte, size)
			copy(want, initial)
			checkBothReadPaths(t, fsIfc, path, want)
		})
	}
}

// TestTruncateGrowIsFree: growing an ext4 file allocates nothing, because the
// new region is a hole. A driver that filled it would work but waste the space
// the format exists to save, so the property is asserted rather than assumed.
func TestTruncateGrowIsFree(t *testing.T) {
	fsIfc := newExt4Image(t, "ext4_4k.img")
	const path = "/grow.bin"
	if err := fsIfc.WriteFile(path, wpattern(100, 5), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	before := blockCount(t, fsIfc, path)
	w := probeWritable(t, mustOpen(t, fsIfc, path))
	if err := w.Truncate(2 << 20); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	if after := blockCount(t, fsIfc, path); after != before {
		t.Fatalf("growing to 2 MiB changed the block count %d → %d — the hole was filled", before, after)
	}
	want := make([]byte, 2<<20)
	copy(want, wpattern(100, 5))
	checkBothReadPaths(t, fsIfc, path, want)
}

func blockCount(t *testing.T, fsIfc filesystem.Filesystem, path string) int {
	t.Helper()
	ef := concreteFile(mustOpen(t, fsIfc, path))
	if ef == nil {
		t.Fatalf("%s has an unexpected File type", path)
	}
	n := 0
	for _, e := range ef.extents {
		n += int(e.Count)
	}
	return n
}

// TestTruncateShrinkFreesBlocks: the mirror property. Shrinking must actually
// release the blocks, or a filesystem written and truncated repeatedly fills
// up with space nothing references.
func TestTruncateShrinkFreesBlocks(t *testing.T) {
	fsIfc := newExt4Image(t, "ext4_4k.img")
	const path = "/shrink.bin"
	if err := fsIfc.WriteFile(path, wpattern(200*1024, 6), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	before := blockCount(t, fsIfc, path)
	w := probeWritable(t, mustOpen(t, fsIfc, path))
	if err := w.Truncate(4096); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	after := blockCount(t, fsIfc, path)
	if after >= before || after != 1 {
		t.Fatalf("shrink left %d blocks (was %d), want 1", after, before)
	}
	// The freed blocks must be reusable: allocate them again and prove no
	// double-booking by writing a second file and reading both back.
	other := wpattern(150*1024, 7)
	if err := fsIfc.WriteFile("/other.bin", other, 0o644); err != nil {
		t.Fatalf("WriteFile other: %v", err)
	}
	checkBothReadPaths(t, fsIfc, "/other.bin", other)
	checkBothReadPaths(t, fsIfc, path, wpattern(200*1024, 6)[:4096])
}

// TestWriteAtConcurrentDisjointRanges: io.WriterAt permits parallel writes to
// non-overlapping ranges, and a mount issues exactly that. Under -race this
// also proves the File's own state is not torn by a concurrent extend.
func TestWriteAtConcurrentDisjointRanges(t *testing.T) {
	fsIfc := newExt4Image(t, "ext4_4k.img")
	const path = "/conc.bin"
	const n, chunk = 16, 4096
	want := wpattern(n*chunk, 0x9E)
	if err := fsIfc.WriteFile(path, make([]byte, n*chunk), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	w := probeWritable(t, mustOpen(t, fsIfc, path))

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			off := int64(i * chunk)
			_, errs[i] = w.WriteAt(want[off:off+chunk], off)
		}()
	}
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = w.ReadAt(make([]byte, 512), 0)
			_ = w.Size()
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent WriteAt %d: %v", i, err)
		}
	}
	checkBothReadPaths(t, fsIfc, path, want)
}

// --- the two layouts this driver refuses to write ------------------------

// TestReadOnlyLayoutsDoNotSatisfyWritableFile is the point of returning a
// separate type: a caller learns at the PROBE that it must fall back, instead
// of discovering it one failed request at a time. Inline data is one such
// layout; the ext2/ext3 indirect block map is the other.
func TestReadOnlyLayoutsDoNotSatisfyWritableFile(t *testing.T) {
	t.Run("inline", func(t *testing.T) {
		fsIfc := newExt4Image(t, "ext4_4k.img")
		f := mustOpen(t, fsIfc, "/small_inline.txt")
		if _, ok := f.(filesystem.WritableFile); ok {
			t.Fatal("an inline-data File satisfies WritableFile — a caller would write into an inode it cannot extend")
		}
		// ...and it must still read correctly, both ways.
		want, err := fsIfc.ReadFile("/small_inline.txt")
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		got := make([]byte, len(want))
		if n, err := f.ReadAt(got, 0); n != len(want) || (err != nil && !errors.Is(err, io.EOF)) {
			t.Fatalf("ReadAt = %d, %v", n, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatal("inline ReadAt disagrees with ReadFile")
		}
		if f.Size() != int64(len(want)) {
			t.Fatalf("Size() = %d, want %d", f.Size(), len(want))
		}
		if err := f.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
	t.Run("block-map", func(t *testing.T) {
		// ext2 has no extents at all, so every regular file on it takes the
		// indirect block map.
		fsIfc := newExt4Image(t, "ext2_1k.img")
		f := mustOpen(t, fsIfc, "/multiblock.bin")
		if _, ok := f.(filesystem.WritableFile); ok {
			t.Fatal("a block-map File satisfies WritableFile — growing one needs indirect blocks this driver never allocates")
		}
		want, err := fsIfc.ReadFile("/multiblock.bin")
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		got := make([]byte, len(want))
		if n, err := f.ReadAt(got, 0); n != len(want) || (err != nil && !errors.Is(err, io.EOF)) {
			t.Fatalf("ReadAt = %d, %v", n, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatal("block-map ReadAt disagrees with ReadFile")
		}
	})
}

// --- contract and error branches -----------------------------------------

func openWritable(t *testing.T, initial []byte) (filesystem.Filesystem, filesystem.WritableFile) {
	t.Helper()
	fsIfc := newExt4Image(t, "ext4_4k.img")
	if err := fsIfc.WriteFile("/w.bin", initial, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return fsIfc, probeWritable(t, mustOpen(t, fsIfc, "/w.bin"))
}

func TestWriteAtContractEdges(t *testing.T) {
	_, w := openWritable(t, wpattern(100, 8))

	// An empty write is a no-op that succeeds; refusing it would break
	// callers that pass a zero-length buffer.
	if n, err := w.WriteAt(nil, 0); n != 0 || err != nil {
		t.Fatalf("WriteAt(empty) = %d, %v, want 0, nil", n, err)
	}
	if n, err := w.WriteAt([]byte("x"), -1); n != 0 || err == nil {
		t.Fatalf("WriteAt(-1) = %d, %v, want an error", n, err)
	}
	// An offset plus a length that overflows int64 must be refused before
	// anything derives a block number from it.
	if _, err := w.WriteAt(make([]byte, 8), math.MaxInt64-2); err == nil {
		t.Fatal("WriteAt with an overflowing end returned nil, want an error")
	}
	if err := w.Truncate(-1); err == nil {
		t.Fatal("Truncate(-1) returned nil, want an error")
	}
	// A write entirely inside the existing map changes no metadata, so the
	// early return in mapRangeLocked is taken.
	if n, err := w.WriteAt([]byte("ab"), 3); n != 2 || err != nil {
		t.Fatalf("in-place WriteAt = %d, %v", n, err)
	}
}

func TestWriteAfterCloseIsRefused(t *testing.T) {
	_, w := openWritable(t, wpattern(100, 9))
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := w.WriteAt([]byte("x"), 0); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("WriteAt after Close = %v, want os.ErrClosed", err)
	}
	if err := w.Truncate(0); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Truncate after Close = %v, want os.ErrClosed", err)
	}
	if err := w.Sync(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Sync after Close = %v, want os.ErrClosed", err)
	}
}

// syncRecorder is a readerWriterAt that HAS a Sync, so the forwarding branch
// is exercised in both outcomes.
type syncRecorder struct {
	blockDevice
	calls int
	err   error
}

func (s *syncRecorder) Sync() error { s.calls++; return s.err }

var errSyncInjected = errors.New("ext4: injected sync failure")

func TestSyncForwardsToTheBackingHandle(t *testing.T) {
	fsIfc, w := openWritable(t, wpattern(10, 10))
	fs := fsIfc.(*ext4FS)
	// The ordinary case: Open uses os.OpenFile, so a real image reaches
	// fsync(2). blockDevice has Sync in its contract, so there is nothing to
	// probe for and nothing that can silently do nothing.
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	rec := &syncRecorder{blockDevice: fs.f}
	fs.f = rec
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if rec.calls != 1 {
		t.Fatalf("Sync forwarded %d times, want 1", rec.calls)
	}
	// A failing fsync must be reported, not swallowed: a server that answered
	// FILE_SYNC on it would be lying about durability.
	rec.err = errSyncInjected
	if err := w.Sync(); !errors.Is(err, errSyncInjected) {
		t.Fatalf("Sync = %v, want the backing error", err)
	}
}

// faultWriteRW fails writes to a chosen byte region, so the write path's I/O
// error branches are reachable without a real disk failure.
type faultWriteRW struct {
	blockDevice
	lo, hi   int64
	skip     int
	hits     int
	failRead bool
}

var errWriteInjected = errors.New("ext4: injected I/O failure")

func (f *faultWriteRW) ReadAt(p []byte, off int64) (int, error) {
	if f.failRead && off >= f.lo && off < f.hi {
		return 0, errWriteInjected
	}
	return f.blockDevice.ReadAt(p, off)
}

func (f *faultWriteRW) WriteAt(p []byte, off int64) (int, error) {
	if f.hi > f.lo && off >= f.lo && off < f.hi {
		f.hits++
		if f.hits > f.skip {
			return 0, errWriteInjected
		}
	}
	return f.blockDevice.WriteAt(p, off)
}

func TestWriteAtIOErrors(t *testing.T) {
	// The payload write itself fails.
	t.Run("payload", func(t *testing.T) {
		fsIfc, w := openWritable(t, wpattern(4096*3, 12))
		fs := fsIfc.(*ext4FS)
		ef := concreteFile(mustOpen(t, fsIfc, "/w.bin"))
		base := fs.partOffset + int64(ef.extents[0].PhysBlock)*int64(fs.sb.BlockSize)
		fs.f = &faultWriteRW{blockDevice: fs.f, lo: base, hi: base + int64(fs.sb.BlockSize)}
		if _, err := w.WriteAt(wpattern(16, 13), 0); !errors.Is(err, errWriteInjected) {
			t.Fatalf("WriteAt with a failing data write = %v", err)
		}
	})
	// Zeroing a freshly allocated block fails: everything past the file's own
	// blocks is fair game, so fail every write beyond them.
	t.Run("zero-new-block", func(t *testing.T) {
		fsIfc, w := openWritable(t, wpattern(100, 14))
		fs := fsIfc.(*ext4FS)
		fs.f = &faultWriteRW{blockDevice: fs.f, lo: 0, hi: 1 << 62}
		if _, err := w.WriteAt(wpattern(8, 15), 500000); !errors.Is(err, errWriteInjected) {
			t.Fatalf("WriteAt with a failing zero-fill = %v", err)
		}
	})
	// Zeroing the slack behind the old end of file fails.
	t.Run("zero-slack", func(t *testing.T) {
		fsIfc, w := openWritable(t, wpattern(100, 16))
		fs := fsIfc.(*ext4FS)
		ef := concreteFile(mustOpen(t, fsIfc, "/w.bin"))
		base := fs.partOffset + int64(ef.extents[0].PhysBlock)*int64(fs.sb.BlockSize)
		fs.f = &faultWriteRW{blockDevice: fs.f, lo: base, hi: base + int64(fs.sb.BlockSize)}
		if err := w.Truncate(3000); !errors.Is(err, errWriteInjected) {
			t.Fatalf("Truncate(grow) with a failing slack zero = %v", err)
		}
	})
	// The inode re-read that commitLocked performs fails.
	t.Run("inode-read", func(t *testing.T) {
		fsIfc, w := openWritable(t, wpattern(100, 17))
		fs := fsIfc.(*ext4FS)
		fs.f = &faultWriteRW{blockDevice: fs.f, lo: 0, hi: 1 << 62, failRead: true}
		if err := w.Truncate(50); !errors.Is(err, errWriteInjected) {
			t.Fatalf("Truncate with an unreadable inode = %v", err)
		}
	})
}

func TestTruncateShrinkIOError(t *testing.T) {
	fsIfc, w := openWritable(t, wpattern(4096*6, 18))
	fs := fsIfc.(*ext4FS)
	ef := concreteFile(mustOpen(t, fsIfc, "/w.bin"))
	// Fail the write that zeroes the slack in the last block kept.
	blk := ef.extents[0].PhysBlock + 1
	base := fs.partOffset + int64(blk)*int64(fs.sb.BlockSize)
	fs.f = &faultWriteRW{blockDevice: fs.f, lo: base, hi: base + int64(fs.sb.BlockSize)}
	if err := w.Truncate(4096 + 7); !errors.Is(err, errWriteInjected) {
		t.Fatalf("Truncate(shrink) with a failing slack zero = %v", err)
	}
}

func TestCoalesceExtents(t *testing.T) {
	// Nothing to merge.
	if got := coalesceExtents(nil); got != nil {
		t.Fatalf("coalesceExtents(nil) = %v", got)
	}
	one := []extentLeaf{{LogBlock: 0, PhysBlock: 5, Count: 1}}
	if got := coalesceExtents(one); len(got) != 1 {
		t.Fatalf("coalesceExtents(one) = %v", got)
	}
	// Out of order, and adjacent both logically and physically: one run.
	got := coalesceExtents([]extentLeaf{
		{LogBlock: 1, PhysBlock: 11, Count: 1},
		{LogBlock: 0, PhysBlock: 10, Count: 1},
		{LogBlock: 2, PhysBlock: 12, Count: 2},
	})
	if len(got) != 1 || got[0].LogBlock != 0 || got[0].PhysBlock != 10 || got[0].Count != 4 {
		t.Fatalf("adjacent runs did not merge: %v", got)
	}
	// Logically adjacent but physically apart: two runs, and the reverse.
	if got := coalesceExtents([]extentLeaf{
		{LogBlock: 0, PhysBlock: 10, Count: 1},
		{LogBlock: 1, PhysBlock: 99, Count: 1},
	}); len(got) != 2 {
		t.Fatalf("physically separate runs merged: %v", got)
	}
	if got := coalesceExtents([]extentLeaf{
		{LogBlock: 0, PhysBlock: 10, Count: 1},
		{LogBlock: 5, PhysBlock: 11, Count: 1},
	}); len(got) != 2 {
		t.Fatalf("logically separate runs merged: %v", got)
	}
	// A merge that would exceed the on-disk 15-bit count field must not
	// happen: the field would wrap and the extent would describe nothing.
	if got := coalesceExtents([]extentLeaf{
		{LogBlock: 0, PhysBlock: 10, Count: 0x7FFE},
		{LogBlock: 0x7FFE, PhysBlock: 10 + 0x7FFE, Count: 4},
	}); len(got) != 2 {
		t.Fatalf("a merge past the 0x7FFF ceiling was allowed: %v", got)
	}
}

func TestSingleExtentChild(t *testing.T) {
	fsIfc := newExt4Image(t, "ext4_4k.img")
	rw := getRW(fsIfc.(*ext4FS))
	fs := fsIfc.(*ext4FS)

	// A small file's extents live inline in the inode: depth 0, so there is
	// no child block to free.
	if err := fsIfc.WriteFile("/tiny.bin", wpattern(10, 19), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	in, err := lookupPath(rw, fs.partOffset, fs.sb, "/tiny.bin")
	if err != nil {
		t.Fatalf("lookupPath: %v", err)
	}
	if _, ok := singleExtentChild(in); ok {
		t.Fatal("an inline extent root reported a child block")
	}
	// A raw inode with no extent magic at all.
	blank := &inode{raw: make([]byte, fs.sb.InodeSize)}
	if _, ok := singleExtentChild(blank); ok {
		t.Fatal("an inode with no extent magic reported a child block")
	}
}
