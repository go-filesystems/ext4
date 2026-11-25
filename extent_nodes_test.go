package filesystem_ext4

import (
	"encoding/binary"
	"io"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

type nodeMem struct{ b []byte }

func (m *nodeMem) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off+int64(len(p)) > int64(len(m.b)) {
		return 0, io.ErrUnexpectedEOF
	}
	return copy(p, m.b[off:]), nil
}
func (m *nodeMem) WriteAt(p []byte, off int64) (int, error) { return copy(m.b[off:], p), nil }

// putNode writes an extent node header and index entries into buf.
func putNode(buf []byte, depth uint16, children ...uint32) {
	le := binary.LittleEndian
	le.PutUint16(buf[0:], ExtentMagic)
	le.PutUint16(buf[2:], uint16(len(children)))
	le.PutUint16(buf[6:], depth)
	for i, c := range children {
		le.PutUint32(buf[12+i*12+4:], c)
	}
}

func TestExtentNodeBlocks(t *testing.T) {
	sb := &superblock{BlockSize: 1024, BlocksCount: 16}
	build := func(rootDepth uint16, rootKids ...uint32) (*nodeMem, *inode) {
		m := &nodeMem{b: make([]byte, 16*1024)}
		in := &inode{num: 12, raw: make([]byte, 256)}
		putNode(in.raw[inodeOffBlock:inodeOffBlock+60], rootDepth, rootKids...)
		return m, in
	}

	// depth 2: root -> index block 3 -> leaf blocks 4 and 5.
	m, in := build(2, 3)
	putNode(m.b[3*1024:], 1, 4, 5)
	putNode(m.b[4*1024:], 0)
	putNode(m.b[5*1024:], 0)
	got, err := extentNodeBlocks(m, 0, sb, in)
	if err != nil || !reflect.DeepEqual(sortedU64(got), []uint64{3, 4, 5}) {
		t.Fatalf("depth 2: %v %v", got, err)
	}

	// depth 0: nothing below the root.
	m, in = build(0)
	if got, err := extentNodeBlocks(m, 0, sb, in); err != nil || len(got) != 0 {
		t.Fatalf("depth 0: %v %v", got, err)
	}

	for name, tc := range map[string]struct {
		depth uint16
		kids  []uint32
	}{
		"too deep":     {maxExtentDepth + 1, []uint32{3}},
		"block zero":   {1, []uint32{0}},
		"out of range": {1, []uint32{16}},
		"twice":        {1, []uint32{3, 3}},
		"unreadable":   {1, []uint32{15}},
	} {
		m, in := build(tc.depth, tc.kids...)
		if name == "unreadable" {
			m.b = m.b[:15*1024] // block 15 lies past the device
		}
		putNode(m.b[3*1024:], 0)
		if got, err := extentNodeBlocks(m, 0, sb, in); err == nil {
			t.Fatalf("%s: no error, got %v", name, got)
		}
	}
}

// readFailNth fails the nth read (1-based) of one block range.
type readFailNth struct {
	*nodeMem
	from, to int64
	nth      int
	seen     int
}

func (r *readFailNth) ReadAt(p []byte, off int64) (int, error) {
	if off >= r.from && off < r.to {
		if r.seen++; r.seen == r.nth {
			return 0, io.ErrUnexpectedEOF
		}
	}
	return r.nodeMem.ReadAt(p, off)
}

// freeInodeBlocks reports the failure of each step that precedes the frees.
func TestFreeInodeBlocksReportsTreeErrors(t *testing.T) {
	sb := &superblock{BlockSize: 1024, BlocksCount: 16}
	newInode := func() *inode {
		in := &inode{num: 12, raw: make([]byte, 256)}
		binary.LittleEndian.PutUint32(in.raw[inodeOffFlags:], InodeFlagExtents)
		return in
	}
	// The root is not an extent node.
	in := newInode()
	if err := freeInodeBlocks(&nodeMem{b: make([]byte, 16*1024)}, 0, sb, in); err == nil {
		t.Fatal("a bad root freed without an error")
	}
	// The tree parses once, then a node cannot be read the second time.
	in = newInode()
	putNode(in.raw[inodeOffBlock:inodeOffBlock+60], 1, 3)
	m := &nodeMem{b: make([]byte, 16*1024)}
	putNode(m.b[3*1024:], 0)
	f := &readFailNth{nodeMem: m, from: 3 * 1024, to: 4 * 1024, nth: 2}
	if err := freeInodeBlocks(f, 0, sb, in); err == nil {
		t.Fatal("a node unreadable at free time was ignored")
	}
}

// failFirstWrite fails the first write and passes the rest, so an error that
// is swallowed cannot be reported again by a later write.
type failFirstWrite struct {
	readerWriterAt
	failAt int // 1-based index of the write that fails; 0 means the first
	seen   int
	failed bool
}

func (w *failFirstWrite) WriteAt(p []byte, off int64) (int, error) {
	if w.failAt == 0 {
		w.failAt = 1
	}
	if w.seen++; w.seen == w.failAt {
		w.failed = true
		return 0, io.ErrShortWrite
	}
	return w.readerWriterAt.WriteAt(p, off)
}

func plainFS(t *testing.T) *ext4FS {
	t.Helper()
	mke2fs := hostTool("mke2fs")
	if mke2fs == "" {
		t.Skip("mke2fs not available")
	}
	img := filepath.Join(t.TempDir(), "img")
	if out, err := exec.Command(mke2fs, "-q", "-t", "ext4", "-O", "^metadata_csum,^uninit_bg", "-b", "1024", img, "8M").CombinedOutput(); err != nil {
		t.Skipf("mke2fs unusable here: %v\n%s", err, out)
	}
	fsys, err := Open(img, -1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fsys.Close() })
	return fsys.(*ext4FS)
}

// A tree node that cannot be released is an error.
func TestFreeInodeBlocksReportsNodeFreeFailure(t *testing.T) {
	fs := plainFS(t)
	const node = 5000
	leaf := make([]byte, 1024)
	putNode(leaf, 0)
	if _, err := fs.f.WriteAt(leaf, fs.partOffset+node*1024); err != nil {
		t.Fatal(err)
	}
	in := &inode{num: 12, raw: make([]byte, 256)}
	binary.LittleEndian.PutUint32(in.raw[inodeOffFlags:], InodeFlagExtents)
	putNode(in.raw[inodeOffBlock:inodeOffBlock+60], 1, node)
	if err := freeInodeBlocks(&failFirstWrite{readerWriterAt: fs.f}, fs.partOffset, fs.sb, in); err == nil {
		t.Fatal("a node block that could not be freed was ignored")
	}
}

// A freed inode that cannot be written back is an error, not a silent success.
func TestDecrementAndFreeInodeReportsWriteFailure(t *testing.T) {
	// Fail each write in turn, on a fresh image: whichever write fails, the
	// deletion must say so. Stop once a run performs fewer writes than n.
	for n := 1; n <= 12; n++ {
		fs := plainFS(t)
		if err := fs.WriteFile("/empty", nil, 0o644); err != nil {
			t.Fatal(err)
		}
		in, err := lookupPath(fs.f, fs.partOffset, fs.sb, "/empty")
		if err != nil {
			t.Fatal(err)
		}
		w := &failFirstWrite{readerWriterAt: fs.f, failAt: n}
		err = decrementAndFreeInode(w, fs.partOffset, fs.sb, in)
		if !w.failed {
			if n == 1 {
				t.Fatal("no write was made")
			}
			return
		}
		if err == nil {
			t.Fatalf("write %d failed and the deletion reported success", n)
		}
	}
}

// Every write a directory removal makes may fail; each failure must surface,
// including the write-back of the scrubbed inode.
func TestRemoveDirTreeReportsEachWriteFailure(t *testing.T) {
	for n := 1; n <= 40; n++ {
		fs := plainFS(t)
		if err := fs.MkDir("/d", 0o755); err != nil {
			t.Fatal(err)
		}
		w := &failFirstWrite{readerWriterAt: fs.f, failAt: n}
		err := removeDirTree(w, fs.partOffset, fs.sb, "/d")
		if !w.failed {
			if n == 1 {
				t.Fatal("no write was made")
			}
			return
		}
		if err == nil {
			t.Fatalf("write %d failed and the removal reported success", n)
		}
	}
	t.Fatal("removal made more than 40 writes: raise the bound")
}

// A freed bit that has no pending want entry was already applied and retired
// by the dispatcher; unreserving it must not wait out the polling bound.
func TestUnreserveWithoutWantReturnsPromptly(t *testing.T) {
	fs := plainFS(t)
	reserveBitmapBits(fs.f, fs.sb, 0, true, []int{5}, false)
	const free = 8000 // a free bit of group 0 on an 8M image
	start := time.Now()
	unreserveBitmapBits(fs.f, fs.partOffset, fs.sb, 0, true, []int{free})
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("unreserve waited %v for a bit nothing was waiting on", d)
	}
}

// A retired bit that is still set on disk (so a default of "cleared" would
// mismatch) must be skipped, whether or not another bit of the call is pending.
func TestUnreserveSkipsRetiredBitsStillSet(t *testing.T) {
	for _, pending := range []bool{false, true} {
		fs := plainFS(t)
		bits := []int{0} // bit 0 of group 0 is allocated on disk, nothing waits on it
		if pending {
			reserveBitmapBits(fs.f, fs.sb, 0, true, []int{8001}, false)
			bits = append(bits, 8001)
		}
		start := time.Now()
		unreserveBitmapBits(fs.f, fs.partOffset, fs.sb, 0, true, bits)
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("pending=%v: unreserve waited %v for a retired bit", pending, d)
		}
	}
}

func sortedU64(s []uint64) []uint64 {
	out := append([]uint64(nil), s...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}
