package filesystem_ext4_test

import (
	"encoding/binary"
	"errors"
	"sync"
	"testing"

	ext4 "github.com/go-filesystems/ext4"
)

// memBlockDevice is an in-memory ext4.BlockDevice used to drive the full
// Open -> ReadFile decode path against fuzzed images without touching disk.
// Writes are accepted but bounded to the existing buffer; the hardening
// targets are all read paths, so a no-grow device is sufficient and keeps a
// malformed image from triggering an unbounded Truncate.
type memBlockDevice struct {
	mu  sync.RWMutex
	buf []byte
}

func newMemBlockDevice(b []byte) *memBlockDevice {
	cp := make([]byte, len(b))
	copy(cp, b)
	return &memBlockDevice{buf: cp}
}

func (d *memBlockDevice) ReadAt(p []byte, off int64) (int, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if off < 0 || off >= int64(len(d.buf)) {
		return 0, errors.New("memBlockDevice: read out of range")
	}
	n := copy(p, d.buf[off:])
	if n < len(p) {
		return n, errors.New("memBlockDevice: short read")
	}
	return n, nil
}

func (d *memBlockDevice) WriteAt(p []byte, off int64) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if off < 0 || off >= int64(len(d.buf)) {
		return 0, errors.New("memBlockDevice: write out of range")
	}
	n := copy(d.buf[off:], p)
	return n, nil
}

func (d *memBlockDevice) Sync() error { return nil }
func (d *memBlockDevice) Size() (int64, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return int64(len(d.buf)), nil
}
func (d *memBlockDevice) Truncate(int64) error { return nil }
func (d *memBlockDevice) Close() error         { return nil }

// validImageBytes formats a small real ext4 image and returns its raw bytes,
// usable as a healthy fuzz seed and as a base to mutate.
func validImageBytes(t testing.TB) []byte {
	t.Helper()
	st, ok := t.(*testing.T)
	if !ok {
		t.Skip("validImageBytes needs *testing.T")
	}
	fs, cleanup := ext4.NewTempFSWithSize(st, 4*1024*1024)
	if err := fs.WriteFile("/seed.txt", []byte("hello hardening world"), 0o644); err != nil {
		cleanup()
		t.Fatalf("seed WriteFile: %v", err)
	}
	h := ext4.CloneFSImage(st, fs)
	cleanup()
	return append([]byte(nil), ext4.HookMemFileBuf(h)...)
}

// -------------------------------------------------------------------------
// FuzzOpenAndRead: end-to-end. Open an arbitrary byte blob as an ext4 device
// and read a path. The only contract is "no panic": every malformed input
// must yield an error (or a clean read), never a crash, OOB, or hang.
// -------------------------------------------------------------------------

func FuzzOpenAndRead(f *testing.F) {
	f.Add(validImageBytes(f))
	// A tiny non-ext4 blob (no partition table, bad magic).
	f.Add(make([]byte, 8192))

	f.Fuzz(func(t *testing.T, img []byte) {
		if len(img) < 2048 {
			return
		}
		dev := newMemBlockDevice(img)
		fs, err := ext4.OpenFromDevice(dev, -1)
		if err != nil {
			return // rejected cleanly
		}
		// Exercise the read/traverse paths; any error is acceptable, a panic
		// is not. Bound the work with a few representative paths.
		for _, p := range []string{"/", "/seed.txt", "/nonexistent", "/a/b/c"} {
			_, _ = fs.ReadFile(p)
			_, _ = fs.ListDir(p)
		}
		_ = fs.Close()
	})
}

// -------------------------------------------------------------------------
// FuzzParseExtentNode: the C1/M1/M3 surface. Feed arbitrary bytes as an
// extent node; assert no panic regardless of magic, depth, entries, or child
// pointers. A self-referential index block must terminate with an error.
// -------------------------------------------------------------------------

func FuzzParseExtentNode(f *testing.F) {
	le := binary.LittleEndian

	// Seed: a well-formed depth-0 leaf header.
	leaf := make([]byte, 60)
	le.PutUint16(leaf[0:], 0xF30A) // ExtentMagic
	le.PutUint16(leaf[2:], 1)      // entries
	le.PutUint16(leaf[4:], 4)      // max
	le.PutUint16(leaf[6:], 0)      // depth
	le.PutUint32(leaf[12:], 0)     // logblock
	le.PutUint16(leaf[16:], 1)     // count
	f.Add(leaf)

	// Seed: a cyclic index node pointing at block 2 (the canonical attack).
	cyc := make([]byte, 60)
	le.PutUint16(cyc[0:], 0xF30A)
	le.PutUint16(cyc[2:], 1)
	le.PutUint16(cyc[4:], 4)
	le.PutUint16(cyc[6:], 1)  // depth 1 -> index node
	le.PutUint32(cyc[12:], 0) // ei_block
	le.PutUint32(cyc[16:], 2) // ei_leaf_lo = block 2
	f.Add(cyc)

	// Seed: empty/short buffer.
	f.Add([]byte{0x0A, 0xF3})

	sb := ext4.NewTestSuperblock(8192, 8192, 16, 4096)
	sb.BlocksCount = 16 * 8192

	f.Fuzz(func(t *testing.T, buf []byte) {
		// Back every index-block read with a device that returns the SAME
		// node bytes for every block: if recursion were unbounded this would
		// overflow the stack. It must instead terminate via the visited-set /
		// depth bound.
		dev := &repeatRW{block: padBlock(buf, int(sb.BlockSize))}
		_, _ = ext4.ParseExtentNode(dev, 0, sb, buf, 2, nil) // must not panic
	})
}

func padBlock(node []byte, blockSize int) []byte {
	if blockSize <= 0 {
		blockSize = 4096
	}
	b := make([]byte, blockSize)
	copy(b, node)
	return b
}

// repeatRW returns the same block bytes for every read, so the extent walker
// following a child pointer reads a node that may point back at itself.
type repeatRW struct{ block []byte }

func (r *repeatRW) ReadAt(p []byte, off int64) (int, error) {
	if len(r.block) == 0 {
		return 0, errors.New("empty block")
	}
	for i := range p {
		p[i] = r.block[int((off+int64(i))%int64(len(r.block)))]
	}
	return len(p), nil
}
func (r *repeatRW) WriteAt(p []byte, off int64) (int, error) { return len(p), nil }

// bytesRW is a read/write view over a byte slice satisfying ext4.ReaderWriterAt
// (ReadSuperblock requires WriteAt as well as ReadAt).
type bytesRW []byte

func (b bytesRW) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= int64(len(b)) {
		return 0, errors.New("bytesRW: read out of range")
	}
	n := copy(p, b[off:])
	if n < len(p) {
		return n, errors.New("bytesRW: short read")
	}
	return n, nil
}

func (b bytesRW) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= int64(len(b)) {
		return 0, errors.New("bytesRW: write out of range")
	}
	return copy(b[off:], p), nil
}

// -------------------------------------------------------------------------
// FuzzParseDirBlock: the H1 surface. Arbitrary directory block bytes must not
// panic on the name_len slice.
// -------------------------------------------------------------------------

func FuzzParseDirBlock(f *testing.F) {
	le := binary.LittleEndian

	good := make([]byte, 64)
	le.PutUint32(good[0:], 2)  // inode
	le.PutUint16(good[4:], 16) // rec_len
	good[6] = 4                // name_len
	good[7] = 2                // file_type
	copy(good[8:], "root")
	f.Add(good)

	// Seed: name_len overruns the record and the block.
	bad := make([]byte, 16)
	le.PutUint32(bad[0:], 2)
	le.PutUint16(bad[4:], 12)
	bad[6] = 0xFF // name_len 255 >> rec_len-8
	bad[7] = 1
	f.Add(bad)

	f.Fuzz(func(t *testing.T, buf []byte) {
		_ = ext4.ParseDirBlock(buf) // must not panic
	})
}

// -------------------------------------------------------------------------
// FuzzReadSuperblock: the C2/H3 surface. Arbitrary 1024+ byte superblock must
// never divide-by-zero or shift into a bad block size.
// -------------------------------------------------------------------------

func FuzzReadSuperblock(f *testing.F) {
	le := binary.LittleEndian

	good := make([]byte, 1024)
	le.PutUint16(good[56:], 0xEF53)
	le.PutUint32(good[24:], 2)    // log_block_size -> 4096
	le.PutUint32(good[32:], 8192) // blocks_per_group
	le.PutUint32(good[40:], 8192) // inodes_per_group
	le.PutUint32(good[0:], 1024)  // inodes_count
	f.Add(good)

	// Seed: InodesPerGroup = 0 (C2).
	z := make([]byte, 1024)
	le.PutUint16(z[56:], 0xEF53)
	le.PutUint32(z[24:], 2)
	le.PutUint32(z[32:], 8192)
	le.PutUint32(z[40:], 0) // inodes_per_group = 0
	f.Add(z)

	// Seed: s_log_block_size = 32 (H3).
	big := make([]byte, 1024)
	le.PutUint16(big[56:], 0xEF53)
	le.PutUint32(big[24:], 32) // log_block_size = 32 -> huge shift
	le.PutUint32(big[32:], 8192)
	le.PutUint32(big[40:], 8192)
	f.Add(big)

	f.Fuzz(func(t *testing.T, buf []byte) {
		// readSuperblock reads 1024 bytes at offset 1024, so place the fuzzed
		// superblock there inside a >=2048-byte image.
		img := make([]byte, 2048)
		copy(img[1024:], buf)
		sb, err := ext4.ReadSuperblock(bytesRW(img), 0)
		if err == nil && sb != nil {
			// Every accepted superblock must have safe divisors / block size.
			if sb.InodesPerGroup == 0 || sb.BlocksPerGroup == 0 || sb.BlockSize == 0 {
				t.Fatalf("accepted superblock with zero divisor/blocksize")
			}
		}
	})
}
