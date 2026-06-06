package filesystem_ext4_test

import (
	"encoding/binary"
	"testing"

	ext4 "github.com/go-filesystems/ext4"
)

// memFile provided by shared test helpers in test_helpers_test.go

func TestZeroEntryInBlockAndZeroDirEntry(t *testing.T) {
	const blockSize = 4096
	sb := &ext4.Superblock{BlockSize: blockSize, InodeSize: 256}

	// Build a directory block with one entry named "foo".
	buf := make([]byte, blockSize)
	le := binary.LittleEndian
	le.PutUint32(buf[0:], 123)  // inode
	le.PutUint16(buf[4:], 4096) // rec_len (fills block)
	buf[0+6] = 3                // name_len
	buf[0+7] = ext4.FtRegFile
	copy(buf[8:], "foo")

	// zeroEntryInBlock should find and zero the inode.
	ok := ext4.ZeroEntryInBlock(buf, "foo", le)
	if !ok {
		t.Fatalf("zeroEntryInBlock failed to find entry")
	}
	if binary.LittleEndian.Uint32(buf[0:]) != 0 {
		t.Fatalf("inode not zeroed in block")
	}

	// Now test zeroDirEntry which reads via extents.
	mf := &memFile{buf: make([]byte, blockSize*8)}
	phys := uint64(3)
	copy(mf.buf[int(phys)*blockSize:], buf)

	dirIno := ext4.NewTestInode(2, 256)
	if err := ext4.SetInlineExtents(dirIno, []ext4.ExtentLeaf{{LogBlock: 0, PhysBlock: phys, Count: 1}}); err != nil {
		t.Fatalf("setInlineExtents: %v", err)
	}

	if err := ext4.ZeroDirEntry(mf, 0, sb, dirIno, "foo"); err != nil {
		t.Fatalf("zeroDirEntry failed: %v", err)
	}
}

func TestFreeInodeSlotAndDecrement(t *testing.T) {
	const blockSize = 4096
	mf := &memFile{buf: make([]byte, blockSize*16)}
	sb := ext4.NewTestSuperblock(16, 32, 1, blockSize)
	sb.DescSize = 32
	sb.InodeSize = 256

	// Write a BGD for group 0 with inode bitmap at block 2.
	g := uint32(0)
	descOff := ext4.BgdOffset(sb, g)
	dRaw := make([]byte, sb.DescSize)
	le := binary.LittleEndian
	le.PutUint32(dRaw[4:], 2) // inode bitmap block
	copy(mf.buf[int(descOff):], dRaw)

	// Set inode bitmap bit for inode number 5 (index 4).
	bmapOff := int(2 * blockSize)
	mf.buf[bmapOff+0] = 1 << 4

	// sb counters
	sb.FreeInodesCount = 10

	// freeInodeSlot should clear the bit for inode 5.
	if err := ext4.FreeInodeSlot(mf, 0, sb, 5); err != nil {
		t.Fatalf("freeInodeSlot: %v", err)
	}
	if mf.buf[bmapOff+0]&(1<<4) != 0 {
		t.Fatalf("freeInodeSlot did not clear bit")
	}

	// Test decrementAndFreeInode for links==0 (no extents path).
	in := ext4.NewTestInode(6, sb.InodeSize)
	// links = 0
	le.PutUint16(ext4.InodeRaw(in)[ext4.InodeOffLinksCount:], 0)

	// Place a bitmap bit for inode 6
	mf.buf[bmapOff+0] |= 1 << 5

	if err := ext4.DecrementAndFreeInode(mf, 0, sb, in); err != nil {
		t.Fatalf("decrementAndFreeInode: %v", err)
	}
	if mf.buf[bmapOff+0]&(1<<5) != 0 {
		t.Fatalf("decrementAndFreeInode did not free inode slot")
	}
}

func TestWriteBitmapWithCsumAndFreeBlock(t *testing.T) {
	const blockSize = 4096
	mf := &memFile{buf: make([]byte, blockSize*32)}
	sb := ext4.NewTestSuperblock(0, 0, 1, blockSize)
	sb.DescSize = 64
	sb.FeatureROCompat = ext4.FeatROCompatMetadataCsum
	sb.FeatureIncompat = ext4.FeatIncompat64bit
	sb.BlocksCount = uint64(128)

	// Create bgd raw with block bitmap at block 3.
	dRaw := make([]byte, sb.DescSize)
	d := ext4.DecodeBGD(dRaw, sb)
	le := binary.LittleEndian
	le.PutUint32(dRaw[0:], 3)

	// Write a bitmap at block 3 with some data.
	bmap := make([]byte, sb.BlockSize)
	bmap[0] = 0xFF
	copy(mf.buf[3*blockSize:], bmap)

	if err := ext4.WriteBitmapWithCsum(mf, 0, sb, 0, d, true); err != nil {
		t.Fatalf("writeBitmapWithCsum: %v", err)
	}
	// Expect checksum fields to be non-zero (lo or hi depending on size).
	lo := binary.LittleEndian.Uint16(dRaw[24:])
	hi := binary.LittleEndian.Uint16(dRaw[56:])
	if lo == 0 && hi == 0 {
		t.Fatalf("bitmap checksum not written")
	}

	// freeBlock: set bitmap bit for physical block 10
	// Map physBlock 10 to group g and bit.
	// Choose FirstDataBlock = 0 for simplicity.
	sb.FirstDataBlock = 0
	sb.BlocksPerGroup = 32
	// Prepare a BGD at table position.
	descOff := ext4.BgdOffset(sb, 0)
	// One descriptor: set block bitmap block to 5.
	d2 := make([]byte, sb.DescSize)
	le.PutUint32(d2[0:], 5)
	copy(mf.buf[int(descOff):], d2)
	// Set block bitmap at block 5: mark phys block 10 as used (bit 10)
	bOff := 5 * blockSize
	mf.buf[bOff+(10/8)] = 1 << uint(10%8)

	if err := ext4.FreeBlock(mf, 0, sb, 10); err != nil {
		t.Fatalf("freeBlock: %v", err)
	}
	if mf.buf[bOff+(10/8)]&(1<<uint(10%8)) != 0 {
		t.Fatalf("freeBlock did not clear the bit")
	}
}
