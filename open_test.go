package filesystem_ext4

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	filesystem "github.com/go-filesystems/interface"
	"github.com/go-volumes/safeio"
)

// probeOpener asserts the capability is reachable the way a caller reaches it —
// through the filesystem.Filesystem interface Open returns, not the concrete
// type — and hands back the Opener.
func probeOpener(t *testing.T, fsIfc filesystem.Filesystem) filesystem.Opener {
	t.Helper()
	o, ok := fsIfc.(filesystem.Opener)
	if !ok {
		t.Fatal("ext4 does not satisfy filesystem.Opener")
	}
	return o
}

// checkAgainstReadFile is the verification that matters: for a file on a real
// mke2fs-produced image, ReadAt must return EXACTLY the corresponding slice of
// what ReadFile returns.
//
// It does this two ways. First it walks the whole file in chunks whose size is
// coprime with every block size in the corpus (777, 4095, 4097), so every read
// straddles block and extent boundaries at a different phase — a dense sweep
// that no off-by-one-block survives. Then it targets the boundaries explicitly:
// each extent's first and last byte, ±1, at several lengths, plus end of file.
func checkAgainstReadFile(t *testing.T, fsIfc filesystem.Filesystem, path string) {
	t.Helper()
	want, err := fsIfc.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	f, err := probeOpener(t, fsIfc).OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile(%s): %v", path, err)
	}
	defer f.Close()

	size := int64(len(want))
	if f.Size() != size {
		t.Fatalf("%s: Size() = %d, want %d (len of ReadFile)", path, f.Size(), size)
	}

	// Dense sweep: read the whole file at every phase.
	for _, chunk := range []int{777, 4095, 4097} {
		got := make([]byte, 0, size)
		buf := make([]byte, chunk)
		for off := int64(0); off < size; off += int64(chunk) {
			n, err := f.ReadAt(buf, off)
			full := off+int64(chunk) <= size
			if full && err != nil {
				t.Fatalf("%s: ReadAt(len=%d, off=%d) err = %v, want nil", path, chunk, off, err)
			}
			if !full && !errors.Is(err, io.EOF) {
				t.Fatalf("%s: ReadAt(len=%d, off=%d) err = %v, want io.EOF", path, chunk, off, err)
			}
			got = append(got, buf[:n]...)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s: chunked ReadAt (chunk=%d) differs from ReadFile", path, chunk)
		}
	}

	// Targeted boundaries: extent edges and end of file.
	offsets := map[int64]bool{0: true, size: true}
	if size > 0 {
		offsets[size-1] = true
	}
	if ef, ok := f.(*ext4File); ok {
		bs := ef.blockSize
		for _, e := range ef.extents {
			for _, b := range []int64{int64(e.LogBlock) * bs, (int64(e.LogBlock) + int64(e.Count)) * bs} {
				for _, o := range []int64{b - 1, b, b + 1} {
					if o >= 0 && o <= size {
						offsets[o] = true
					}
				}
			}
		}
	}
	for off := range offsets {
		for _, l := range []int{1, 5, 1023, 1024, 4096, 8193} {
			p := make([]byte, l)
			n, err := f.ReadAt(p, off)
			short := off+int64(l) > size
			wantN := l
			if short {
				wantN = int(size - off)
			}
			if n != wantN {
				t.Fatalf("%s: ReadAt(len=%d, off=%d) n = %d, want %d", path, l, off, n, wantN)
			}
			if short {
				if !errors.Is(err, io.EOF) {
					t.Fatalf("%s: ReadAt(len=%d, off=%d) err = %v, want io.EOF", path, l, off, err)
				}
			} else if err != nil {
				t.Fatalf("%s: ReadAt(len=%d, off=%d) err = %v, want nil", path, l, off, err)
			}
			if !bytes.Equal(p[:n], want[off:off+int64(n)]) {
				t.Fatalf("%s: ReadAt(len=%d, off=%d) bytes differ from ReadFile[%d:%d]", path, l, off, off, off+int64(n))
			}
		}
	}

	// io.SectionReader is the consumer the io.ReaderAt contract protects.
	got, err := io.ReadAll(io.NewSectionReader(f, 0, size))
	if err != nil {
		t.Fatalf("%s: ReadAll(SectionReader): %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s: SectionReader round-trip differs from ReadFile", path)
	}
}

// TestOpenFileFixtureCorpus is the real-image proof. The corpus is produced by
// the e2fsprogs oracle (mke2fs, see testdata/gen.go), not by this package, and
// spans every layout the read path has: extent-mapped ext4 at 4 KiB and 1 KiB
// blocks, the classic ext2/ext3 indirect block map (including the
// double-indirect range for the 300 000-byte file), inline data, and a sparse
// file whose 1 MiB hole is not backed by any block at all.
func TestOpenFileFixtureCorpus(t *testing.T) {
	dir := fixtureDir(t)
	for _, img := range goodImages {
		t.Run(img, func(t *testing.T) {
			fsIfc, err := Open(filepath.Join(dir, img), -1)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer fsIfc.Close()

			for _, p := range []string{
				"/empty.txt",
				"/small_inline.txt",
				"/multiblock.bin",
				"/big.bin",
				"/sub/deep/nested.bin",
				"/sparse.bin",
			} {
				checkAgainstReadFile(t, fsIfc, p)
			}

			// Report which internal layout each file actually took, so the
			// test is not silently exercising one path four times.
			for _, p := range []string{"/small_inline.txt", "/big.bin", "/sparse.bin"} {
				f, err := probeOpener(t, fsIfc).OpenFile(p)
				if err != nil {
					t.Fatalf("OpenFile(%s): %v", p, err)
				}
				ef := f.(*ext4File)
				t.Logf("%s %s: inline=%v extents=%d size=%d blockSize=%d",
					img, p, ef.inline != nil, len(ef.extents), ef.size, ef.blockSize)
				f.Close()
			}
		})
	}
}

// TestOpenFileSparseHole targets the case a block-indexed implementation gets
// wrong: 1 MiB of file that is backed by no block at all, followed by three
// real bytes. ReadAt inside the hole must produce zeros without touching the
// device, and a read straddling the hole's end must stitch zeros to "END".
func TestOpenFileSparseHole(t *testing.T) {
	dir := fixtureDir(t)
	for _, img := range goodImages {
		t.Run(img, func(t *testing.T) {
			fsIfc, err := Open(filepath.Join(dir, img), -1)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer fsIfc.Close()
			f, err := probeOpener(t, fsIfc).OpenFile("/sparse.bin")
			if err != nil {
				t.Fatalf("OpenFile: %v", err)
			}
			defer f.Close()

			const holeSize = 1 << 20
			if f.Size() != holeSize+3 {
				t.Fatalf("Size() = %d, want %d", f.Size(), holeSize+3)
			}
			// Deep inside the hole.
			p := make([]byte, 4096)
			if n, err := f.ReadAt(p, 512*1024); n != 4096 || err != nil {
				t.Fatalf("ReadAt in hole = %d, %v", n, err)
			}
			if !bytes.Equal(p, make([]byte, 4096)) {
				t.Fatal("ReadAt in hole returned non-zero bytes")
			}
			// Straddling the hole's end: zeros then END, then io.EOF.
			q := make([]byte, 8)
			n, err := f.ReadAt(q, holeSize-3)
			if n != 6 || !errors.Is(err, io.EOF) {
				t.Fatalf("ReadAt across hole end = %d, %v; want 6, io.EOF", n, err)
			}
			if !bytes.Equal(q[:6], []byte{0, 0, 0, 'E', 'N', 'D'}) {
				t.Fatalf("across hole end got % x, want 00 00 00 'E' 'N' 'D'", q[:6])
			}
		})
	}
}

// TestOpenFileEOFSemantics pins the io.ReaderAt end-of-file rules. A short read
// with a nil error is the failure mode that breaks io.SectionReader silently.
func TestOpenFileEOFSemantics(t *testing.T) {
	dir := fixtureDir(t)
	fsIfc, err := Open(filepath.Join(dir, "ext4_4k.img"), -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fsIfc.Close()

	for _, path := range []string{"/small_inline.txt", "/multiblock.bin"} {
		t.Run(path, func(t *testing.T) {
			want, err := fsIfc.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			f, err := probeOpener(t, fsIfc).OpenFile(path)
			if err != nil {
				t.Fatalf("OpenFile: %v", err)
			}
			defer f.Close()
			size := int64(len(want))

			p := make([]byte, 8)
			if n, err := f.ReadAt(p, 4); n != 8 || err != nil {
				t.Fatalf("ReadAt(8,4) = %d, %v", n, err)
			}
			if !bytes.Equal(p, want[4:12]) {
				t.Fatal("ReadAt bytes differ from ReadFile")
			}
			// Straddling the end: bytes AND io.EOF.
			if n, err := f.ReadAt(p, size-3); n != 3 || !errors.Is(err, io.EOF) {
				t.Fatalf("ReadAt straddling end = %d, %v; want 3, io.EOF", n, err)
			}
			// At and past Size(): 0, io.EOF.
			if n, err := f.ReadAt(p, size); n != 0 || !errors.Is(err, io.EOF) {
				t.Fatalf("ReadAt at Size() = %d, %v; want 0, io.EOF", n, err)
			}
			if n, err := f.ReadAt(p, 1<<40); n != 0 || !errors.Is(err, io.EOF) {
				t.Fatalf("ReadAt past Size() = %d, %v; want 0, io.EOF", n, err)
			}
			// Zero-length read inside the file is a full read.
			if n, err := f.ReadAt(nil, 1); n != 0 || err != nil {
				t.Fatalf("ReadAt(empty,1) = %d, %v; want 0, nil", n, err)
			}
			// Negative offset errors instead of panicking.
			if n, err := f.ReadAt(p, -1); n != 0 || err == nil {
				t.Fatalf("ReadAt(-1) = %d, %v; want an error", n, err)
			}
			// Close is idempotent; a read after it fails loudly.
			if err := f.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			if err := f.Close(); err != nil {
				t.Fatalf("second Close: %v", err)
			}
			if n, err := f.ReadAt(p, 0); n != 0 || !errors.Is(err, os.ErrClosed) {
				t.Fatalf("ReadAt after Close = %d, %v; want 0, os.ErrClosed", n, err)
			}
		})
	}
}

// TestOpenFileRejects covers the refusal paths: a directory and a path that
// does not resolve, each failing the way ReadFile does.
func TestOpenFileRejects(t *testing.T) {
	dir := fixtureDir(t)
	fsIfc, err := Open(filepath.Join(dir, "ext4_4k.img"), -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fsIfc.Close()
	o := probeOpener(t, fsIfc)

	for _, tc := range []struct{ name, path string }{
		{"directory", "/sub"},
		{"root", "/"},
		{"missing", "/nope.bin"},
		{"missing parent", "/does/not/exist"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if f, err := o.OpenFile(tc.path); err == nil {
				f.Close()
				t.Fatalf("OpenFile(%q) succeeded, want an error", tc.path)
			}
		})
	}

	// A symlink resolves to its target, as ReadFile does.
	f, err := o.OpenFile("/fast.lnk")
	if err != nil {
		t.Fatalf("OpenFile(symlink): %v", err)
	}
	defer f.Close()
	want, err := fsIfc.ReadFile("/fast.lnk")
	if err != nil {
		t.Fatalf("ReadFile(symlink): %v", err)
	}
	if f.Size() != int64(len(want)) {
		t.Fatalf("Size() through symlink = %d, want %d", f.Size(), len(want))
	}
}

// TestOpenFileConcurrentReads exercises the concurrency guarantee io.ReaderAt
// makes and a mount depends on: many ReadAt calls in flight on one File.
func TestOpenFileConcurrentReads(t *testing.T) {
	dir := fixtureDir(t)
	fsIfc, err := Open(filepath.Join(dir, "ext4_4k.img"), -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fsIfc.Close()
	want, err := fsIfc.ReadFile("/big.bin")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	f, err := probeOpener(t, fsIfc).OpenFile("/big.bin")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer f.Close()

	var wg sync.WaitGroup
	errCh := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			off := int64(i) * 9377
			if off > f.Size() {
				off = f.Size()
			}
			p := make([]byte, 5000+i)
			n, err := f.ReadAt(p, off)
			if err != nil && !errors.Is(err, io.EOF) {
				errCh <- fmt.Errorf("goroutine %d: %w", i, err)
				return
			}
			if !bytes.Equal(p[:n], want[off:off+int64(n)]) {
				errCh <- fmt.Errorf("goroutine %d: bytes differ at off=%d", i, off)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

// TestOpenFileWrittenFile checks the capability on a file this package writes
// itself, on a filesystem it formats itself — the write path's own output read
// back through the offset path.
func TestOpenFileWrittenFile(t *testing.T) {
	path := makeFormattedImage(t)
	defer os.Remove(path)
	fsIfc, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fsIfc.Close()

	body := make([]byte, 40000)
	for i := range body {
		body[i] = byte(i*17 + 3)
	}
	if err := fsIfc.WriteFile("/written.bin", body, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	checkAgainstReadFile(t, fsIfc, "/written.bin")

	f, err := probeOpener(t, fsIfc).OpenFile("/written.bin")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer f.Close()
	got := make([]byte, 3000)
	if n, err := f.ReadAt(got, 20000); n != 3000 || err != nil {
		t.Fatalf("ReadAt(3000, 20000) = %d, %v", n, err)
	}
	if !bytes.Equal(got, body[20000:23000]) {
		t.Fatal("ReadAt bytes differ from the bytes written")
	}
}

// --- corrupt / defensive paths --------------------------------------------

// TestNewFileSizeExceedsFilesystem covers the i_size ceiling. i_size is
// attacker-controlled; a file cannot be larger than the filesystem holding it,
// and ReadFile refuses such an inode, so OpenFile must too — otherwise the two
// disagree about which inodes are readable.
func TestNewFileSizeExceedsFilesystem(t *testing.T) {
	dir := fixtureDir(t)
	fsIfc, err := Open(filepath.Join(dir, "ext4_4k.img"), -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fsIfc.Close()
	fs := fsIfc.(*ext4FS)

	orig := fs.sb.BlocksCount
	defer func() { fs.sb.BlocksCount = orig }()

	// One block of filesystem: /big.bin's 300 000-byte i_size cannot fit.
	fs.sb.BlocksCount = 1
	if _, err := fs.OpenFile("/big.bin"); !errors.Is(err, safeio.ErrTooLarge) {
		t.Fatalf("OpenFile with tiny BlocksCount = %v, want safeio.ErrTooLarge", err)
	}
	// ReadFile refuses it the same way, so the two paths agree.
	if _, err := fsIfc.ReadFile("/big.bin"); !errors.Is(err, safeio.ErrTooLarge) {
		t.Fatalf("ReadFile with tiny BlocksCount = %v, want safeio.ErrTooLarge", err)
	}
}

// TestNewFileUnknownFilesystemSize covers the BlocksCount == 0 fallback: with
// no filesystem size to bound by, the ceiling comes from the highest logical
// byte the extents can address, exactly as readFileData derives it.
func TestNewFileUnknownFilesystemSize(t *testing.T) {
	dir := fixtureDir(t)
	fsIfc, err := Open(filepath.Join(dir, "ext4_4k.img"), -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fsIfc.Close()
	fs := fsIfc.(*ext4FS)

	orig := fs.sb.BlocksCount
	defer func() { fs.sb.BlocksCount = orig }()
	fs.sb.BlocksCount = 0

	f, err := fs.OpenFile("/multiblock.bin")
	if err != nil {
		t.Fatalf("OpenFile with BlocksCount=0: %v", err)
	}
	defer f.Close()
	if f.Size() != 5000 {
		t.Fatalf("Size() = %d, want 5000", f.Size())
	}
	// The block-range validation is skipped when BlocksCount is 0 (there is
	// nothing to validate against), so the read still succeeds.
	p := make([]byte, 5000)
	if n, err := f.ReadAt(p, 0); n != 5000 || err != nil {
		t.Fatalf("ReadAt = %d, %v", n, err)
	}
	if !bytes.Equal(p, detFile(2, 5000)) {
		t.Fatal("ReadAt bytes differ from the staged content")
	}
}

// TestNewFileSizeWithoutExtents covers the corrupt case readFileData also
// refuses: an extent-mapped inode declaring a size but carrying no extents.
func TestNewFileSizeWithoutExtents(t *testing.T) {
	dir := fixtureDir(t)
	fsIfc, err := Open(filepath.Join(dir, "ext4_4k.img"), -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fsIfc.Close()
	fs := fsIfc.(*ext4FS)
	rw := getRW(fs)

	in, err := lookupPath(rw, fs.partOffset, fs.sb, "/big.bin")
	if err != nil {
		t.Fatalf("lookupPath: %v", err)
	}
	if in.flags()&InodeFlagExtents == 0 {
		t.Skip("fixture inode is not extent-mapped")
	}
	// Blank the extent header's entry count in the in-memory inode only: the
	// tree now declares zero leaves while i_size still claims 300 000 bytes.
	in.raw[inodeOffBlock+2] = 0
	in.raw[inodeOffBlock+3] = 0
	if _, err := fs.newFile(rw, in); err == nil {
		t.Fatal("newFile on a sized inode with no extents: want an error")
	}
}

// TestReadAtBlockOutOfRange covers the M3 guard on the read path: an extent
// pointing outside the filesystem must be refused before an offset is computed
// from it, not read from wherever it lands.
func TestReadAtBlockOutOfRange(t *testing.T) {
	dir := fixtureDir(t)
	fsIfc, err := Open(filepath.Join(dir, "ext4_4k.img"), -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fsIfc.Close()
	fs := fsIfc.(*ext4FS)

	f := &ext4File{
		fs:        fs,
		extents:   []extentLeaf{{LogBlock: 0, PhysBlock: uint64(fs.sb.BlocksCount) + 10, Count: 1}},
		size:      int64(fs.sb.BlockSize),
		blockSize: int64(fs.sb.BlockSize),
		inodeNum:  99,
	}
	n, err := f.ReadAt(make([]byte, 16), 0)
	if err == nil {
		t.Fatal("ReadAt over an out-of-range extent: want an error")
	}
	if n != 0 {
		t.Fatalf("ReadAt n = %d, want 0", n)
	}
}

// TestReadAtTrailingHole covers the hole branch taken when the file's map ends
// before i_size does: every offset past the last extent is a hole and reads as
// zeros, with no next extent to clip against.
func TestReadAtTrailingHole(t *testing.T) {
	dir := fixtureDir(t)
	fsIfc, err := Open(filepath.Join(dir, "ext4_4k.img"), -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fsIfc.Close()
	fs := fsIfc.(*ext4FS)

	// One real block (borrowed from /multiblock.bin) followed by two blocks
	// of nothing.
	real, err := fs.OpenFile("/multiblock.bin")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer real.Close()
	src := real.(*ext4File)
	if len(src.extents) == 0 {
		t.Skip("fixture file has no extents")
	}
	bs := int64(fs.sb.BlockSize)
	f := &ext4File{
		fs:        fs,
		extents:   []extentLeaf{{LogBlock: 0, PhysBlock: src.extents[0].PhysBlock, Count: 1}},
		size:      3 * bs,
		blockSize: bs,
		inodeNum:  src.inodeNum,
	}
	p := make([]byte, 3*bs)
	n, err := f.ReadAt(p, 0)
	if int64(n) != 3*bs || err != nil {
		t.Fatalf("ReadAt = %d, %v; want %d, nil", n, err, 3*bs)
	}
	if !bytes.Equal(p[:bs], detFile(2, 5000)[:bs]) {
		t.Fatal("first block differs from the staged content")
	}
	if !bytes.Equal(p[bs:], make([]byte, 2*bs)) {
		t.Fatal("trailing hole did not read as zeros")
	}
}

// readFailDevice fails every read at or past failFrom so a data-read error can
// be injected at an exact image offset.
type readFailDevice struct {
	*osFileDevice
	failFrom int64
}

func (d *readFailDevice) ReadAt(p []byte, off int64) (int, error) {
	if off >= d.failFrom {
		return 0, io.ErrUnexpectedEOF
	}
	return d.osFileDevice.ReadAt(p, off)
}

// TestReadAtDeviceError covers the I/O error branch: the failure must surface,
// wrapped, rather than become a silent short read.
func TestReadAtDeviceError(t *testing.T) {
	dir := fixtureDir(t)
	imgPath := filepath.Join(dir, "ext4_4k.img")
	fsIfc, err := Open(imgPath, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	fs := fsIfc.(*ext4FS)
	f, err := fs.OpenFile("/multiblock.bin")
	if err != nil {
		fsIfc.Close()
		t.Fatalf("OpenFile: %v", err)
	}
	src := f.(*ext4File)
	if len(src.extents) == 0 {
		fsIfc.Close()
		t.Skip("fixture file has no extents")
	}
	// Fail from the file's first data block onwards, after the map is built.
	failFrom := fs.partOffset + int64(src.extents[0].PhysBlock)*int64(fs.sb.BlockSize)
	f.Close()
	fsIfc.Close()

	raw, err := os.OpenFile(imgPath, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	dev := &readFailDevice{osFileDevice: &osFileDevice{f: raw}, failFrom: 1 << 62}
	fsIfc2, err := OpenFromDevice(dev, -1)
	if err != nil {
		raw.Close()
		t.Fatalf("OpenFromDevice: %v", err)
	}
	defer fsIfc2.Close()
	f2, err := fsIfc2.(*ext4FS).OpenFile("/multiblock.bin")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer f2.Close()
	dev.failFrom = failFrom
	n, err := f2.ReadAt(make([]byte, 64), 0)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("ReadAt err = %v, want io.ErrUnexpectedEOF", err)
	}
	if n != 0 {
		t.Fatalf("ReadAt n = %d, want 0", n)
	}
}

// TestOpenFileInodeReadError covers OpenFile's failure to re-read the inode
// under the inode lock.
func TestOpenFileInodeReadError(t *testing.T) {
	dir := fixtureDir(t)
	imgPath := filepath.Join(dir, "ext4_4k.img")
	raw, err := os.OpenFile(imgPath, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open image: %v", err)
	}
	dev := &readFailDevice{osFileDevice: &osFileDevice{f: raw}, failFrom: 1 << 62}
	fsIfc, err := OpenFromDevice(dev, -1)
	if err != nil {
		raw.Close()
		t.Fatalf("OpenFromDevice: %v", err)
	}
	defer fsIfc.Close()
	fs := fsIfc.(*ext4FS)

	// Warm the path lookup, then fail every subsequent read so the re-read
	// of the inode under the inode lock cannot succeed.
	if _, err := fs.OpenFile("/multiblock.bin"); err != nil {
		t.Fatalf("warm-up OpenFile: %v", err)
	}
	dev.failFrom = 0
	if _, err := fs.OpenFile("/multiblock.bin"); err == nil {
		t.Fatal("OpenFile with a failing device: want an error")
	}
}

// TestOpenFileInodeReReadError covers the failure of the inode re-read that
// OpenFile performs under the inode lock. The re-read and the path lookup that
// precedes it both touch the inode table, so a device-level fault cannot fail
// one without the other; the package seam is driven directly instead.
func TestOpenFileInodeReReadError(t *testing.T) {
	dir := fixtureDir(t)
	fsIfc, err := Open(filepath.Join(dir, "ext4_4k.img"), -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fsIfc.Close()

	orig := openFileReadInode
	t.Cleanup(func() { openFileReadInode = orig })
	sentinel := errors.New("forced inode read failure")
	openFileReadInode = func(readerWriterAt, int64, *superblock, uint32) (*inode, error) {
		return nil, sentinel
	}
	if _, err := fsIfc.(*ext4FS).OpenFile("/multiblock.bin"); !errors.Is(err, sentinel) {
		t.Fatalf("OpenFile = %v, want the forced failure", err)
	}
}

// TestNewFileInlineOverflowError covers the inline branch's error path: an
// inline inode whose declared size overruns what i_block plus the system.data
// xattr can supply is corrupt, and inlineData says so.
func TestNewFileInlineOverflowError(t *testing.T) {
	dir := fixtureDir(t)
	fsIfc, err := Open(filepath.Join(dir, "ext4_4k.img"), -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fsIfc.Close()
	fs := fsIfc.(*ext4FS)
	rw := getRW(fs)

	in, err := lookupPath(rw, fs.partOffset, fs.sb, "/small_inline.txt")
	if err != nil {
		t.Fatalf("lookupPath: %v", err)
	}
	if !in.isInline() {
		t.Skip("fixture inode does not use inline data")
	}
	// In-memory only: claim far more inline bytes than the inode can hold.
	in.setSize(1 << 20)
	if _, err := fs.newFile(rw, in); err == nil {
		t.Fatal("newFile on an over-declared inline inode: want an error")
	}
}

// TestNewFileExtentParseError covers the readExtents failure branch: a corrupt
// extent header must abort OpenFile rather than yield a File with an empty map.
func TestNewFileExtentParseError(t *testing.T) {
	dir := fixtureDir(t)
	fsIfc, err := Open(filepath.Join(dir, "ext4_4k.img"), -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fsIfc.Close()
	fs := fsIfc.(*ext4FS)
	rw := getRW(fs)

	in, err := lookupPath(rw, fs.partOffset, fs.sb, "/big.bin")
	if err != nil {
		t.Fatalf("lookupPath: %v", err)
	}
	if in.flags()&InodeFlagExtents == 0 {
		t.Skip("fixture inode is not extent-mapped")
	}
	// In-memory only: break the extent header magic.
	in.raw[inodeOffBlock] = 0
	in.raw[inodeOffBlock+1] = 0
	if _, err := fs.newFile(rw, in); err == nil {
		t.Fatal("newFile on a corrupt extent header: want an error")
	}
}
