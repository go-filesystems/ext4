package filesystem_ext4_test

import (
	"bytes"
	"encoding/binary"
	"testing"

	ext4 "github.com/go-filesystems/ext4"
)

func TestAllocInodeAndAllocBlocks(t *testing.T) {
	const blockSize = 4096
	const descSize = 32

	mf := &memFile{buf: make([]byte, blockSize*128)}
	sb := ext4.NewTestSuperblock(16, 32, 1, blockSize)

	g := uint32(0)
	descOff := ext4.BgdOffset(sb, g)
	dRaw := make([]byte, sb.DescSize)
	le := binary.LittleEndian
	le.PutUint32(dRaw[0:], 2)
	le.PutUint32(dRaw[4:], 3)
	le.PutUint32(dRaw[8:], 4)
	le.PutUint16(dRaw[12:], 20)
	le.PutUint16(dRaw[14:], 10)
	copy(mf.buf[int(descOff):], dRaw)

	sb.FreeInodesCount = 10
	sb.FreeBlocksCount = 20

	d, err := ext4.ReadBGD(mf, 0, sb, 0)
	if err != nil {
		t.Fatalf("readBGD sanity check failed: %v", err)
	}
	if d.FreeInodesCount == 0 {
		t.Fatalf("sanity: BGD free inodes = 0")
	}
	bmap, err := ext4.ReadBitmap(mf, 0, sb, d.InodeBitmapBlock)
	if err != nil {
		t.Fatalf("readBitmap sanity check failed: %v", err)
	}
	bit, ok := ext4.FindFreeBit(bmap, int(sb.InodesPerGroup))
	if !ok {
		t.Fatalf("sanity: expected a free bit in inode bitmap, got none; bitmap len=%d", len(bmap))
	}
	t.Logf("sanity: inode bitmap first-free-bit=%d", bit)

	ino, err := ext4.AllocInode(mf, 0, sb, false)
	if err != nil {
		t.Fatalf("allocInode failed: %v", err)
	}
	if ino == 0 {
		t.Fatalf("allocInode returned 0")
	}

	blocks, err := ext4.AllocBlocks(mf, 0, sb, 3)
	if err != nil {
		t.Fatalf("allocBlocks failed: %v", err)
	}
	if len(blocks) != 3 {
		t.Fatalf("allocBlocks: expected 3 blocks, got %d", len(blocks))
	}
}

func TestParseExtentNode_IndexDepth(t *testing.T) {
	const blockSize = 4096
	mf := &memFile{buf: make([]byte, blockSize*64)}
	sb := &ext4.Superblock{BlockSize: blockSize}

	childBlock := uint64(10)
	phys := uint64(20)
	childBuf := make([]byte, blockSize)
	le := binary.LittleEndian
	le.PutUint16(childBuf[0:], ext4.ExtentMagic)
	le.PutUint16(childBuf[2:], 1)
	le.PutUint16(childBuf[6:], 0)
	entOff := 12
	le.PutUint32(childBuf[entOff+0:], 0)
	le.PutUint16(childBuf[entOff+4:], uint16(2))
	le.PutUint16(childBuf[entOff+6:], uint16(phys>>32))
	le.PutUint32(childBuf[entOff+8:], uint32(phys&0xFFFFFFFF))
	copy(mf.buf[int(childBlock*uint64(blockSize)):], childBuf)

	parentBuf := make([]byte, 12+12)
	le.PutUint16(parentBuf[0:], ext4.ExtentMagic)
	le.PutUint16(parentBuf[2:], 1)
	le.PutUint16(parentBuf[6:], 1)
	e := parentBuf[12:]
	le.PutUint32(e[4:], uint32(childBlock&0xFFFFFFFF))
	le.PutUint16(e[8:], uint16(childBlock>>32))

	leaves, err := ext4.ParseExtentNode(mf, 0, sb, parentBuf, 42, nil)
	if err != nil {
		t.Fatalf("parseExtentNode(index) failed: %v", err)
	}
	if len(leaves) == 0 {
		t.Fatalf("expected leaf extents from index node")
	}
	if leaves[0].PhysBlock != phys || leaves[0].Count != 2 {
		t.Fatalf("unexpected leaf extent: %+v", leaves[0])
	}
}

func TestReadFileData_InlineExtents(t *testing.T) {
	const blockSize = 512
	mf := &memFile{buf: make([]byte, blockSize*64)}
	sb := ext4.NewTestSuperblock(0, 0, 1, blockSize)

	phys := uint64(7)
	content := []byte("hello-inline-extents")
	blockOff := int(phys) * blockSize
	copy(mf.buf[blockOff:blockOff+len(content)], content)

	in := ext4.NewTestInode(3, 256)
	if err := ext4.SetInlineExtents(in, []ext4.ExtentLeaf{{LogBlock: 0, PhysBlock: phys, Count: 1}}); err != nil {
		t.Fatalf("SetInlineExtents: %v", err)
	}
	ext4.SetSize(in, uint64(len(content)))

	got, err := ext4.ReadFileData(mf, 0, sb, in)
	if err != nil {
		t.Fatalf("readFileData: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("readFileData content mismatch: got %q want %q", got, content)
	}
}

func TestUpdateDirBlockCsum_MetadataCsum(t *testing.T) {
	const blockSize = 4096
	sb := ext4.NewTestSuperblock(0, 0, 1, blockSize)
	sb.FeatureROCompat = ext4.FeatROCompatMetadataCsum
	dirIno := ext4.NewTestInode(42, 256)
	binary.LittleEndian.PutUint32(ext4.InodeRaw(dirIno)[ext4.InodeOffGeneration:], 7)

	buf := make([]byte, blockSize)
	ext4.UpdateDirBlockCsum(buf, sb, dirIno)
	tailOff := len(buf) - 12
	csum := binary.LittleEndian.Uint32(buf[tailOff+8:])
	if csum == 0 {
		t.Fatalf("expected non-zero dir block csum when metadata_csum enabled")
	}
}

func TestWriteBitmapBuf_MetadataCsum64(t *testing.T) {
	const blockSize = 4096
	sb := ext4.NewTestSuperblock(0, 0, 1, blockSize)
	sb.DescSize = 64
	sb.FeatureROCompat = ext4.FeatROCompatMetadataCsum
	mf := &memFile{buf: make([]byte, blockSize*64)}

	dRaw := make([]byte, sb.DescSize)
	d := ext4.DecodeBGD(dRaw, sb)
	d.BlockBitmapBlock = 5
	bmap := make([]byte, sb.BlockSize)

	if err := ext4.WriteBitmapBuf(mf, 0, sb, 0, d, true, d.BlockBitmapBlock, bmap); err != nil {
		t.Fatalf("writeBitmapBuf: %v", err)
	}
	lo := binary.LittleEndian.Uint16(dRaw[24:])
	hi := binary.LittleEndian.Uint16(dRaw[56:])
	if lo == 0 && hi == 0 {
		t.Fatalf("expected bitmap checksum fields to be written for 64-byte descriptors")
	}
}
