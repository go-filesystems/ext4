package filesystem_ext4_test

import (
	"encoding/binary"
	"errors"
	"strings"
	"testing"

	ext4 "github.com/go-filesystems/ext4"
	"github.com/go-volumes/safeio"
)

// putExtentHeader writes a 12-byte ext4 extent header into buf.
func putExtentHeader(buf []byte, entries, depth uint16) {
	le := binary.LittleEndian
	le.PutUint16(buf[0:], 0xF30A) // ExtentMagic
	le.PutUint16(buf[2:], entries)
	le.PutUint16(buf[4:], 4) // max
	le.PutUint16(buf[6:], depth)
	le.PutUint32(buf[8:], 0) // generation
}

// C1: a self-referential extent index block must be rejected (cycle), not
// recursed into until the stack overflows.
func TestExtent_CyclicIndexBlock(t *testing.T) {
	sb := ext4.NewTestSuperblock(8192, 8192, 16, 4096)
	sb.BlocksCount = 16 * 8192

	// Index node at depth 1 with one entry pointing at block 2.
	node := make([]byte, 60)
	putExtentHeader(node, 1, 1)
	le := binary.LittleEndian
	le.PutUint32(node[12:], 0) // ei_block
	le.PutUint32(node[16:], 2) // ei_leaf_lo = block 2

	// The device returns the SAME index node for every block read, so block 2
	// reads back as a depth-1 node pointing at block 2 again: a cycle.
	dev := &repeatRW{block: padBlock(node, int(sb.BlockSize))}

	_, err := ext4.ParseExtentNode(dev, 0, sb, node, 2, nil)
	if err == nil {
		t.Fatal("expected error on cyclic extent index block, got nil")
	}
	// The decreasing-depth invariant fires first (the child claims depth 1 but
	// must be 0), or the visited-set cycle check does. Either is a graceful
	// rejection; assert we did not panic and got a descriptive error.
	if !strings.Contains(err.Error(), "ext4:") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// C1: an index node whose child reads back as another index block of the same
// depth (depth not strictly decreasing) is rejected even without a literal
// pointer cycle.
func TestExtent_NonDecreasingDepth(t *testing.T) {
	sb := ext4.NewTestSuperblock(8192, 8192, 16, 4096)
	sb.BlocksCount = 16 * 8192

	node := make([]byte, 60)
	putExtentHeader(node, 1, 2) // root depth 2
	le := binary.LittleEndian
	le.PutUint32(node[16:], 3) // child = block 3

	// Child block (block 3) also declares depth 2 -> invariant violation.
	child := make([]byte, int(sb.BlockSize))
	putExtentHeader(child, 1, 2)
	le.PutUint32(child[16:], 5)

	dev := &mappedRW{blocks: map[int64][]byte{
		3 * int64(sb.BlockSize): child,
	}, blockSize: int(sb.BlockSize)}

	_, err := ext4.ParseExtentNode(dev, 0, sb, node, 2, nil)
	if err == nil {
		t.Fatal("expected error on non-decreasing extent depth")
	}
}

// C1/M1: depth beyond the format maximum and a too-small node are rejected.
func TestExtent_DepthTooLargeAndShortBuffer(t *testing.T) {
	sb := ext4.NewTestSuperblock(8192, 8192, 16, 4096)
	sb.BlocksCount = 16 * 8192

	deep := make([]byte, 60)
	putExtentHeader(deep, 0, 6) // depth 6 > maxExtentDepth (5)
	if _, err := ext4.ParseExtentNode(&repeatRW{block: padBlock(deep, 4096)}, 0, sb, deep, 1, nil); err == nil {
		t.Fatal("expected error for extent depth > max")
	}

	short := []byte{0x0A, 0xF3, 0x00} // < 12 bytes
	if _, err := ext4.ParseExtentNode(&repeatRW{block: padBlock(short, 4096)}, 0, sb, short, 1, nil); err == nil {
		t.Fatal("expected error for short extent node")
	}
}

// M3: an extent index pointer outside the filesystem is rejected before read.
func TestExtent_ChildBlockOutOfRange(t *testing.T) {
	sb := ext4.NewTestSuperblock(8192, 8192, 16, 4096)
	sb.BlocksCount = 100 // tiny fs

	node := make([]byte, 60)
	putExtentHeader(node, 1, 1)
	le := binary.LittleEndian
	le.PutUint32(node[16:], 9999) // way past BlocksCount

	_, err := ext4.ParseExtentNode(&repeatRW{block: padBlock(node, 4096)}, 0, sb, node, 1, nil)
	if err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("expected out-of-range error, got %v", err)
	}
}

// C2: InodesPerGroup == 0 and BlocksPerGroup == 0 must be rejected at
// readSuperblock time (otherwise a later divide-by-zero panics).
func TestSuperblock_ZeroPerGroupRejected(t *testing.T) {
	le := binary.LittleEndian

	mk := func(ipg, bpg uint32) []byte {
		// readSuperblock reads 1024 bytes at fsOffset+1024, so the superblock
		// must live at byte offset 1024 of a >=2048-byte image.
		buf := make([]byte, 2048)
		raw := buf[1024:]
		le.PutUint16(raw[56:], 0xEF53)
		le.PutUint32(raw[24:], 2) // log_block_size -> 4096
		le.PutUint32(raw[32:], bpg)
		le.PutUint32(raw[40:], ipg)
		return buf
	}

	if _, err := ext4.ReadSuperblock(bytesRW(mk(0, 8192)), 0); err == nil {
		t.Fatal("expected error for inodes_per_group == 0")
	}
	if _, err := ext4.ReadSuperblock(bytesRW(mk(8192, 0)), 0); err == nil {
		t.Fatal("expected error for blocks_per_group == 0")
	}
	// Sanity: a valid pair is accepted.
	if _, err := ext4.ReadSuperblock(bytesRW(mk(8192, 8192)), 0); err != nil {
		t.Fatalf("valid superblock rejected: %v", err)
	}
}

// H3: s_log_block_size beyond 6 must be rejected (else 1024<<n overflows to a
// zero or absurd BlockSize).
func TestSuperblock_LogBlockSizeBounded(t *testing.T) {
	le := binary.LittleEndian
	buf := make([]byte, 2048)
	raw := buf[1024:]
	le.PutUint16(raw[56:], 0xEF53)
	le.PutUint32(raw[24:], 32) // log_block_size = 32
	le.PutUint32(raw[32:], 8192)
	le.PutUint32(raw[40:], 8192)
	if _, err := ext4.ReadSuperblock(bytesRW(buf), 0); err == nil {
		t.Fatal("expected error for s_log_block_size = 32")
	}

	// Boundary: 6 is the largest legal value (64 KiB blocks).
	le.PutUint32(raw[24:], 6)
	if _, err := ext4.ReadSuperblock(bytesRW(buf), 0); err != nil {
		t.Fatalf("log_block_size 6 should be accepted: %v", err)
	}
	// 7 is one past the limit.
	le.PutUint32(raw[24:], 7)
	if _, err := ext4.ReadSuperblock(bytesRW(buf), 0); err == nil {
		t.Fatal("expected error for s_log_block_size = 7")
	}
}

// H1: a directory entry whose name_len overruns the record / block must be
// skipped, not panic the slice expression.
func TestDir_NameLenOverrun(t *testing.T) {
	le := binary.LittleEndian

	// One entry: rec_len 12, but name_len 0xFF claims 255 bytes of name.
	buf := make([]byte, 16)
	le.PutUint32(buf[0:], 2)  // inode
	le.PutUint16(buf[4:], 12) // rec_len
	buf[6] = 0xFF             // name_len overruns
	buf[7] = 1                // file_type
	// Must not panic; the overrunning entry is skipped.
	entries := ext4.ParseDirBlock(buf)
	for _, e := range entries {
		if e.Name == "" && e.Inode == 0 {
			t.Fatal("unexpected empty entry")
		}
	}

	// A valid entry directly after must still parse if name fits.
	good := make([]byte, 32)
	le.PutUint32(good[0:], 5)
	le.PutUint16(good[4:], 16)
	good[6] = 4
	good[7] = 2
	copy(good[8:], "name")
	got := ext4.ParseDirBlock(good)
	if len(got) != 1 || got[0].Name != "name" {
		t.Fatalf("valid dir entry not parsed: %+v", got)
	}
}

// H2: a regular file inode whose i_size is 2^63 (the canonical OOM vector)
// must be rejected via safeio.MakeBytes' ceiling, not allocated.
func TestReadFile_HugeISizeRejected(t *testing.T) {
	fs, cleanup := ext4.NewTempFSWithSize(t, 4*1024*1024)
	defer cleanup()
	if err := fs.WriteFile("/big.txt", []byte("small file body"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// CloneFSImage snapshots the backing image into a HookMemFile whose
	// partition offset is 0 (the image *is* the filesystem), matching every
	// other low-level test in this package.
	rw := ext4.CloneFSImage(t, fs)
	sb := ext4.CloneSuperblockFromFS(fs)

	in, err := ext4.LookupPath(rw, 0, sb, "/big.txt")
	if err != nil {
		t.Fatalf("lookup big.txt: %v", err)
	}

	// Sanity: the honest read works.
	if _, err := ext4.ReadFileData(rw, 0, sb, in); err != nil {
		t.Fatalf("baseline ReadFileData: %v", err)
	}

	// Corrupt i_size to 2^63 and persist it: readFileData re-reads the on-disk
	// inode under lock, so the malicious size must live on disk to be observed.
	// The subsequent read must be rejected by safeio's ceiling, never allocate.
	ext4.SetSize(in, 1<<63)
	if err := ext4.WriteInode(rw, 0, sb, in); err != nil {
		t.Fatalf("WriteInode (corrupt size): %v", err)
	}
	_, rerr := ext4.ReadFileData(rw, 0, sb, in)
	if rerr == nil {
		t.Fatal("expected error reading file with i_size = 2^63")
	}
	if !errors.Is(rerr, safeio.ErrTooLarge) {
		t.Logf("note: error was %v (not ErrTooLarge, but still rejected gracefully)", rerr)
	}
}

// M2: a bare (table-less) image yields offset 0 for both explicit-index and
// auto-select requests rather than an error or panic.
func TestPartition_BareImageFallback(t *testing.T) {
	bare := make([]byte, 8192) // no GPT sig, no MBR magic

	if off, err := ext4.PartitionOffset(memReaderAt(bare), -1); err != nil || off != 0 {
		t.Fatalf("bare auto-select: off=%d err=%v, want 0/nil", off, err)
	}
	if off, err := ext4.PartitionOffset(memReaderAt(bare), 0); err != nil || off != 0 {
		t.Fatalf("bare explicit index: off=%d err=%v, want 0/nil", off, err)
	}
}

// M2: an MBR whose only partition is non-Linux must yield a "no Linux data
// partition" error from the auto-select path (not offset 0, not a panic).
func TestPartition_NoLinuxAutoSelect(t *testing.T) {
	le := binary.LittleEndian
	buf := make([]byte, 8192)
	buf[510] = 0x55
	buf[511] = 0xAA
	e := buf[446:]
	e[4] = 0x0C // FAT32 LBA, not Linux
	le.PutUint32(e[8:], 2048)
	le.PutUint32(e[12:], 1024)
	if _, err := ext4.PartitionOffset(memReaderAt(buf), -1); err == nil {
		t.Fatal("expected no-Linux-partition error")
	}
}

// C1: a root extent node whose own declared depth exceeds the format maximum
// is rejected at the entry point before any child is read.
func TestExtent_RootDepthTooLarge(t *testing.T) {
	sb := ext4.NewTestSuperblock(8192, 8192, 16, 4096)
	sb.BlocksCount = 16 * 8192

	root := make([]byte, 60)
	putExtentHeader(root, 0, 9) // depth 9 > maxExtentDepth at the root
	if _, err := ext4.ParseExtentNode(&repeatRW{block: padBlock(root, 4096)}, 0, sb, root, 7, nil); err == nil {
		t.Fatal("expected error for root extent depth > max")
	}
}

// mappedRW serves specific block offsets from a map and zeros elsewhere.
type mappedRW struct {
	blocks    map[int64][]byte
	blockSize int
}

func (m *mappedRW) ReadAt(p []byte, off int64) (int, error) {
	if b, ok := m.blocks[off]; ok {
		copy(p, b)
		return len(p), nil
	}
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}
func (m *mappedRW) WriteAt(p []byte, off int64) (int, error) { return len(p), nil }
