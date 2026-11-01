package filesystem_ext4

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/go-volumes/safeio"
)

// Memory-bomb regression tests: every input-driven allocation must be refused
// BEFORE it happens. Each test builds a tiny hostile structure (a few KiB) and
// asserts both the error and that the whole call allocated less than 1 MiB,
// measured with runtime.MemStats.TotalAlloc (a refusal that allocates first and
// errors afterwards is not a refusal).

const bombAllocLimit = 1 << 20

// allocDelta returns the bytes allocated (cumulative, not live) while f runs.
func allocDelta(f func()) uint64 {
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	f()
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc
}

// bombFile is a read-only device with real io.ReaderAt semantics: a read that
// starts past the end, or runs over it, reports io.EOF instead of panicking
// (memFile, the older test double, slices unchecked).
type bombFile struct{ buf []byte }

func (b *bombFile) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= int64(len(b.buf)) {
		return 0, io.EOF
	}
	n := copy(p, b.buf[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (b *bombFile) WriteAt(p []byte, off int64) (int, error) { return 0, errors.New("read-only") }

func bombInode(flags uint32, size uint64) *inode {
	raw := make([]byte, 256)
	le := binary.LittleEndian
	le.PutUint16(raw[inodeOffMode:], 0x8000)
	le.PutUint32(raw[inodeOffFlags:], flags)
	return &inode{raw: raw, num: 2, size: size, mode: 0x8000}
}

// putLeafRoot writes a depth-0 extent root holding the given leaves (at most 4)
// into an inode.
func putLeafRoot(in *inode, leaves ...extentLeaf) {
	le := binary.LittleEndian
	b := in.raw[inodeOffBlock : inodeOffBlock+60]
	le.PutUint16(b[0:], ExtentMagic)
	le.PutUint16(b[2:], uint16(len(leaves)))
	le.PutUint16(b[4:], 4)
	le.PutUint16(b[6:], 0)
	for i, l := range leaves {
		o := 12 + i*12
		le.PutUint32(b[o:], l.LogBlock)
		le.PutUint16(b[o+4:], l.Count)
		le.PutUint16(b[o+6:], uint16(l.PhysBlock>>32))
		le.PutUint32(b[o+8:], uint32(l.PhysBlock))
	}
}

// A block-mapped file declaring 2 GiB must not allocate 2 GiB.
func TestBomb_BlockMapDataHugeSize(t *testing.T) {
	backing := make([]byte, 64*1024)
	in := bombInode(0, 1<<31)
	sb := &superblock{BlockSize: 1024, InodeSize: 256, BlocksCount: 1 << 40}
	var err error
	got := allocDelta(func() { _, err = readFileData(&memFile{buf: backing}, 0, sb, in) })
	t.Logf("allocated %d bytes", got)
	if !errors.Is(err, safeio.ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if got > bombAllocLimit {
		t.Fatalf("allocated %d bytes before refusing", got)
	}
}

// i_size >= 2^63 became a negative int and make() panicked.
func TestBomb_BlockMapDataSizeOverflowNoPanic(t *testing.T) {
	backing := make([]byte, 64*1024)
	for _, size := range []uint64{1 << 63, 1<<64 - 1} {
		in := bombInode(0, size)
		sb := &superblock{BlockSize: 1024, InodeSize: 256, BlocksCount: 1 << 40}
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("size %d panicked: %v", size, r)
				}
			}()
			if _, err := readFileData(&memFile{buf: backing}, 0, sb, in); err == nil {
				t.Fatalf("size %d: no error", size)
			}
		}()
	}
}

// aliasedBlockMap builds a 4 KiB-block image whose double-indirect block
// points 1024 times at the same single-indirect block, which points 1024
// times at the same data block: 1M logical blocks mapped by three blocks.
func aliasedBlockMap() (backing []byte, in *inode, sb *superblock) {
	const bs = 4096
	le := binary.LittleEndian
	backing = make([]byte, 16*bs)
	for i := 0; i < bs/4; i++ {
		le.PutUint32(backing[1*bs+i*4:], 2) // double -> same single
		le.PutUint32(backing[2*bs+i*4:], 3) // single -> same data
	}
	in = bombInode(0, uint64(12+1024+1024*1024)*bs)
	le.PutUint32(in.raw[inodeOffBlock+13*4:], 1)
	sb = &superblock{BlockSize: bs, InodeSize: 256, BlocksCount: 16}
	return
}

func TestBomb_BlockMapExtentsAliased(t *testing.T) {
	backing, in, sb := aliasedBlockMap()
	var err error
	got := allocDelta(func() { _, err = in.blockMapExtents(&memFile{buf: backing}, 0, sb) })
	t.Logf("allocated %d bytes", got)
	if err == nil {
		t.Fatal("aliased block map accepted")
	}
	if got > bombAllocLimit {
		t.Fatalf("allocated %d bytes before refusing", got)
	}
}

// A directory whose extent leaves all cover the same blocks read each block
// once per leaf and appended every entry again.
func TestBomb_DirExtentsOverlap(t *testing.T) {
	const bs = 1024
	le := binary.LittleEndian
	backing := make([]byte, 1100*bs)
	// Blocks 10..1099: one full directory block each (85 twelve-byte entries).
	for b := 10; b < 1100; b++ {
		for e := 0; e < bs/12; e++ {
			o := b*bs + e*12
			le.PutUint32(backing[o:], 5)
			rec := 12
			if e == bs/12-1 {
				rec = bs - e*12
			}
			le.PutUint16(backing[o+4:], uint16(rec))
			backing[o+6] = 1
			backing[o+7] = 1
			backing[o+8] = 'a'
		}
	}
	// Index block 5 holding 20 leaves, every one covering blocks 10..1099
	// of the image at a different logical position.
	node := backing[5*bs:]
	le.PutUint16(node[0:], ExtentMagic)
	le.PutUint16(node[2:], 20)
	le.PutUint16(node[4:], 84)
	le.PutUint16(node[6:], 0)
	for i := 0; i < 20; i++ {
		o := 12 + i*12
		le.PutUint32(node[o:], uint32(i*1090))
		le.PutUint16(node[o+4:], 1090)
		le.PutUint16(node[o+6:], 0)
		le.PutUint32(node[o+8:], 10)
	}
	in := bombInode(InodeFlagExtents, 20*1090*bs)
	in.mode = 0x4000
	b := in.raw[inodeOffBlock : inodeOffBlock+60]
	le.PutUint16(b[0:], ExtentMagic)
	le.PutUint16(b[2:], 1)
	le.PutUint16(b[4:], 4)
	le.PutUint16(b[6:], 1)
	le.PutUint32(b[12+4:], 5) // ei_leaf_lo
	sb := &superblock{BlockSize: bs, InodeSize: 256, BlocksCount: 1100}
	var err error
	var n int
	got := allocDelta(func() {
		var es []DirEntry
		es, err = readDir(&memFile{buf: backing}, 0, sb, in)
		n = len(es)
	})
	t.Logf("allocated %d bytes, %d entries", got, n)
	if err == nil {
		t.Fatalf("overlapping directory extents accepted (%d entries)", n)
	}
	if got > bombAllocLimit {
		t.Fatalf("allocated %d bytes before refusing", got)
	}
}

// An extent-mapped file whose i_size is within the (declared) filesystem size
// but far beyond what the image can hold.
func TestBomb_ReadFileDataDeclaredFsHuge(t *testing.T) {
	const bs = 1024
	backing := make([]byte, 64*bs)
	in := bombInode(InodeFlagExtents, 2<<30)
	putLeafRoot(in, extentLeaf{LogBlock: 0, Count: 1, PhysBlock: 10})
	sb := &superblock{BlockSize: bs, InodeSize: 256, BlocksCount: 1 << 40}
	var err error
	got := allocDelta(func() { _, err = readFileData(&memFile{buf: backing}, 0, sb, in) })
	t.Logf("allocated %d bytes", got)
	if !errors.Is(err, safeio.ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if got > bombAllocLimit {
		t.Fatalf("allocated %d bytes before refusing", got)
	}
}

// extentFile builds an extent-mapped file of `blocks` consecutive 1 KiB data
// blocks starting at physical block 10, in an image of 64 blocks.
func extentFile(size uint64, blocks uint16, declared uint64) (*memFile, *inode, *superblock) {
	in := bombInode(InodeFlagExtents, size)
	putLeafRoot(in, extentLeaf{LogBlock: 0, Count: blocks, PhysBlock: 10})
	backing := make([]byte, 64*1024)
	for i := 10 * 1024; i < len(backing); i++ {
		backing[i] = 'x'
	}
	return &memFile{buf: backing}, in, &superblock{BlockSize: 1024, InodeSize: 256, BlocksCount: declared}
}

// The ceiling is exact: i_size == limit is read, limit+1 is refused, for
// extent-mapped and block-mapped files alike, with the limit set by
// SetMaxReadSize.
func TestBomb_MaxReadSizeExact(t *testing.T) {
	const limit = 4096
	t.Run("extents", func(t *testing.T) {
		f, in, sb := extentFile(limit, 4, 1<<40)
		fs := &ext4FS{sb: sb}
		fs.SetMaxReadSize(limit)
		if fs.MaxReadSize() != limit {
			t.Fatalf("MaxReadSize = %d", fs.MaxReadSize())
		}
		got, err := readFileData(f, 0, sb, in)
		if err != nil || len(got) != limit {
			t.Fatalf("at the limit: len=%d err=%v", len(got), err)
		}
		in.size = limit + 1
		if _, err = readFileData(f, 0, sb, in); !errors.Is(err, safeio.ErrTooLarge) {
			t.Fatalf("one over the limit: err = %v, want ErrTooLarge", err)
		}
	})
	t.Run("blockmap", func(t *testing.T) {
		f, _, sb := extentFile(0, 0, 1<<40)
		in := bombInode(0, limit)
		for i := 0; i < 4; i++ {
			binary.LittleEndian.PutUint32(in.raw[inodeOffBlock+i*4:], uint32(10+i))
		}
		fs := &ext4FS{sb: sb}
		fs.SetMaxReadSize(limit)
		got, err := readFileData(f, 0, sb, in)
		if err != nil || len(got) != limit || got[limit-1] != 'x' {
			t.Fatalf("at the limit: len=%d err=%v", len(got), err)
		}
		in.size = limit + 1
		if _, err = readFileData(f, 0, sb, in); !errors.Is(err, safeio.ErrTooLarge) {
			t.Fatalf("one over the limit: err = %v, want ErrTooLarge", err)
		}
	})
	t.Run("restore default", func(t *testing.T) {
		fs := &ext4FS{sb: &superblock{}}
		fs.SetMaxReadSize(-5)
		if fs.MaxReadSize() != DefaultMaxReadSize {
			t.Fatalf("MaxReadSize = %d, want default", fs.MaxReadSize())
		}
	})
}

// The declared filesystem size is also an exact ceiling (8 blocks of 1 KiB).
func TestBomb_DeclaredSizeExact(t *testing.T) {
	f, in, sb := extentFile(8192, 7, 8)
	putLeafRoot(in, extentLeaf{LogBlock: 0, Count: 7, PhysBlock: 1}) // blocks 1..7, then a hole
	if _, err := readFileData(f, 0, sb, in); err != nil {
		t.Fatalf("at the declared size: %v", err)
	}
	in.size = 8193
	if _, err := readFileData(f, 0, sb, in); !errors.Is(err, safeio.ErrTooLarge) {
		t.Fatalf("one over: err = %v, want ErrTooLarge", err)
	}
}

// declaredBytes must not wrap: 2^63 blocks of 1 KiB overflow an int64.
func TestBomb_DeclaredBytesOverflow(t *testing.T) {
	sb := &superblock{BlockSize: 1024, BlocksCount: 1 << 63}
	if n, ok := sb.declaredBytes(); !ok || n <= 0 {
		t.Fatalf("declaredBytes = %d, %v", n, ok)
	}
	if got := sb.fileSizeCeiling(0); got != DefaultMaxReadSize {
		t.Fatalf("ceiling = %d, want the read limit", got)
	}
	if _, ok := (&superblock{BlockSize: 1024}).declaredBytes(); ok {
		t.Fatal("zero BlocksCount reported as known")
	}
}

// The block budget is exact: the pointers an inode may name are the blocks the
// device holds, indirect blocks included.
func TestBomb_BlockMapBudgetExact(t *testing.T) {
	const bs = 1024
	le := binary.LittleEndian
	build := func(indirect int) (*memFile, *inode) {
		backing := make([]byte, 40*bs)
		for i := 0; i < indirect; i++ {
			le.PutUint32(backing[20*bs+i*4:], uint32(30+i))
		}
		in := bombInode(0, uint64(12+indirect)*bs)
		for i := 0; i < 12; i++ {
			le.PutUint32(in.raw[inodeOffBlock+i*4:], uint32(1+i))
		}
		le.PutUint32(in.raw[inodeOffBlock+12*4:], 20)
		return &memFile{buf: backing}, in
	}
	sb := &superblock{BlockSize: bs, InodeSize: 256, BlocksCount: 14}
	// 12 direct + the indirect block + 1 indirect entry = 14 charged.
	f, in := build(1)
	leaves, err := in.blockMapExtents(f, 0, sb)
	if err != nil {
		t.Fatalf("at the budget: %v", err)
	}
	if len(leaves) != 2 { // blocks 1..12 coalesce, 30 stands alone
		t.Fatalf("leaves = %+v", leaves)
	}
	// One more pointer is 15 charged.
	f, in = build(2)
	if _, err = in.blockMapExtents(f, 0, sb); !errors.Is(err, safeio.ErrTooLarge) {
		t.Fatalf("one over the budget: err = %v, want ErrTooLarge", err)
	}
	// The device size tightens the budget below the declared count.
	sb2 := &superblock{BlockSize: bs, InodeSize: 256, BlocksCount: 1 << 40, deviceBytes: 14 * bs}
	f, in = build(2)
	if _, err = in.blockMapExtents(f, 0, sb2); !errors.Is(err, safeio.ErrTooLarge) {
		t.Fatalf("device bound: err = %v, want ErrTooLarge", err)
	}
	// With neither known the read limit decides (synthetic superblocks).
	sb3 := &superblock{BlockSize: bs, InodeSize: 256}
	if got, want := sb3.blockBudget(), uint64(DefaultMaxReadSize)/bs; got != want {
		t.Fatalf("budget = %d, want %d", got, want)
	}
}

// A sparse block-mapped file declaring a petabyte costs nothing for its holes:
// the walk skips them instead of probing every logical block.
func TestBomb_BlockMapSparseHugeIsCheap(t *testing.T) {
	in := bombInode(0, 1<<50)
	binary.LittleEndian.PutUint32(in.raw[inodeOffBlock:], 7)
	sb := &superblock{BlockSize: 1024, InodeSize: 256, BlocksCount: 1 << 40}
	var leaves []extentLeaf
	var err error
	got := allocDelta(func() { leaves, err = in.blockMapExtents(&memFile{buf: make([]byte, 8192)}, 0, sb) })
	if err != nil || len(leaves) != 1 || leaves[0].PhysBlock != 7 {
		t.Fatalf("leaves=%+v err=%v", leaves, err)
	}
	if got > bombAllocLimit {
		t.Fatalf("allocated %d bytes", got)
	}
}

func TestBomb_ValidateExtents(t *testing.T) {
	ok := [][]extentLeaf{
		nil,
		{{LogBlock: 0, Count: 4, PhysBlock: 10}},
		{{LogBlock: 0, Count: 4, PhysBlock: 10}, {LogBlock: 4, Count: 4, PhysBlock: 14}},   // adjacent
		{{LogBlock: 8, Count: 4, PhysBlock: 30}, {LogBlock: 0, Count: 4, PhysBlock: 10}},   // disordered, disjoint
		{{LogBlock: 0, Count: 4, PhysBlock: 10}, {LogBlock: 2, Count: 0, PhysBlock: 10}},   // zero length maps nothing
		{{LogBlock: 0, Count: 4, PhysBlock: 10}, {LogBlock: 100, Count: 4, PhysBlock: 20}}, // sparse
		{{LogBlock: 0, Count: 32768, PhysBlock: 1 << 33}, {LogBlock: 32768, Count: 1, PhysBlock: 5}},
	}
	for i, l := range ok {
		if err := validateExtents(l, 2); err != nil {
			t.Errorf("ok[%d]: %v", i, err)
		}
	}
	bad := [][]extentLeaf{
		{{LogBlock: 0, Count: 4, PhysBlock: 10}, {LogBlock: 3, Count: 4, PhysBlock: 50}},   // logical overlap by one
		{{LogBlock: 0, Count: 4, PhysBlock: 10}, {LogBlock: 10, Count: 4, PhysBlock: 13}},  // physical overlap by one
		{{LogBlock: 10, Count: 4, PhysBlock: 50}, {LogBlock: 0, Count: 11, PhysBlock: 10}}, // disordered, overlapping
		{{LogBlock: 0, Count: 4, PhysBlock: 10}, {LogBlock: 0, Count: 4, PhysBlock: 10}},   // identical
	}
	for i, l := range bad {
		if err := validateExtents(l, 2); err == nil {
			t.Errorf("bad[%d]: accepted", i)
		}
	}
	// The caller's order is preserved.
	l := []extentLeaf{{LogBlock: 8, Count: 4, PhysBlock: 30}, {LogBlock: 0, Count: 4, PhysBlock: 10}}
	_ = validateExtents(l, 2)
	if l[0].LogBlock != 8 {
		t.Fatal("validateExtents reordered its argument")
	}
}

// An inline extent root with overlapping leaves is refused at parse time.
func TestBomb_ParseExtentRootOverlap(t *testing.T) {
	in := bombInode(InodeFlagExtents, 8192)
	putLeafRoot(in, extentLeaf{LogBlock: 0, Count: 4, PhysBlock: 10}, extentLeaf{LogBlock: 2, Count: 4, PhysBlock: 40})
	sb := &superblock{BlockSize: 1024, InodeSize: 256, BlocksCount: 64}
	if _, err := in.extents(&memFile{buf: make([]byte, 64*1024)}, 0, sb); err == nil {
		t.Fatal("overlapping extents accepted")
	}
}

// The journal checks sit exactly at the bytes the file holds.
func TestBomb_JournalReserveExact(t *testing.T) {
	if left, err := journalReserveDescs(3*descSize, 3); err != nil || left != 0 {
		t.Fatalf("exact fit: left=%d err=%v", left, err)
	}
	if _, err := journalReserveDescs(3*descSize-1, 3); !errors.Is(err, safeio.ErrTooLarge) {
		t.Fatalf("one byte short: err = %v", err)
	}
	// nDesc*descSize must not wrap on any platform.
	if _, err := journalReserveDescs(1<<30, 1<<32-1); !errors.Is(err, safeio.ErrTooLarge) {
		t.Fatalf("max nDesc: err = %v", err)
	}
	if rem, err := journalRemaining(writeJournalHeader(t, 0)); err != nil || rem != txHeaderSize {
		t.Fatalf("remaining = %d, %v", rem, err)
	}
}

// A well-formed transaction still replays after the checks went in.
func TestBomb_JournalValidTransactionStillReplays(t *testing.T) {
	le := binary.LittleEndian
	payload := []byte("0123456789abcdef")
	var b []byte
	b = le.AppendUint32(b, journalMagic)
	b = le.AppendUint64(b, 7)
	b = le.AppendUint32(b, 1)
	b = le.AppendUint64(b, 2) // block 2
	b = le.AppendUint32(b, uint32(len(payload)))
	b = append(b, payload...)
	crc := crc32.Update(0, castagnoli, b)
	b = le.AppendUint32(b, commitMagic)
	b = le.AppendUint64(b, 7)
	b = le.AppendUint32(b, crc)
	p := filepath.Join(t.TempDir(), "img.journal")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img := &memFile{buf: make([]byte, 8192)}
	j := &Journal{js: f, enabled: true, f: img, blockSize: 1024, sb: &superblock{BlocksCount: 8, BlockSize: 1024}}
	if err := j.initNextSeq(); err != nil || j.nextSeq != 8 {
		t.Fatalf("initNextSeq: nextSeq=%d err=%v", j.nextSeq, err)
	}
	if err := j.ReplayOnOpen(); err != nil {
		t.Fatalf("ReplayOnOpen: %v", err)
	}
	if string(img.buf[2048:2048+len(payload)]) != string(payload) {
		t.Fatalf("payload not applied: %q", img.buf[2048:2048+len(payload)])
	}
}

func writeJournalHeader(t *testing.T, nDesc uint32, descs ...[2]uint64) *os.File {
	t.Helper()
	p := filepath.Join(t.TempDir(), "img.journal")
	var b []byte
	le := binary.LittleEndian
	b = le.AppendUint32(b, journalMagic)
	b = le.AppendUint64(b, 1)
	b = le.AppendUint32(b, nDesc)
	for _, d := range descs {
		b = le.AppendUint64(b, d[0])
		b = le.AppendUint32(b, uint32(d[1]))
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestBomb_JournalDescriptorCount(t *testing.T) {
	for _, name := range []string{"initNextSeq", "ReplayOnOpen"} {
		t.Run(name, func(t *testing.T) {
			f := writeJournalHeader(t, 1<<24)
			j := &Journal{js: f, enabled: true, f: &memFile{}, blockSize: 1024, sb: &superblock{BlocksCount: 10}}
			got := allocDelta(func() {
				if name == "initNextSeq" {
					_ = j.initNextSeq()
				} else {
					_ = j.ReplayOnOpen()
				}
			})
			if got > bombAllocLimit {
				t.Fatalf("allocated %d bytes for a 16-byte journal", got)
			}
		})
	}
}

func TestBomb_JournalDataLength(t *testing.T) {
	for _, name := range []string{"initNextSeq", "ReplayOnOpen"} {
		t.Run(name, func(t *testing.T) {
			f := writeJournalHeader(t, 1, [2]uint64{1, 1 << 30})
			j := &Journal{js: f, enabled: true, f: &memFile{}, blockSize: 1024, sb: &superblock{BlocksCount: 10}}
			got := allocDelta(func() {
				if name == "initNextSeq" {
					_ = j.initNextSeq()
				} else {
					_ = j.ReplayOnOpen()
				}
			})
			if got > bombAllocLimit {
				t.Fatalf("allocated %d bytes for a 28-byte journal", got)
			}
		})
	}
}
