package filesystem_ext4_test

import (
	"bytes"
	"encoding/binary"
	"testing"

	ext4 "github.com/go-filesystems/ext4"
)

func TestSuperblock_numBlockGroups_Exact(t *testing.T) {
	sb := ext4.NewTestSuperblock(256, 8192, 4, 4096)
	if got := ext4.NumBlockGroups(sb); got != 4 {
		t.Errorf("numBlockGroups: got %d, want 4", got)
	}
}

func TestSuperblock_numBlockGroups_Partial(t *testing.T) {
	sb := &ext4.Superblock{BlocksCount: uint64(3*8192 + 100), BlocksPerGroup: 8192}
	if got := ext4.NumBlockGroups(sb); got != 4 {
		t.Errorf("numBlockGroups (partial): got %d, want 4", got)
	}
}

func TestSuperblock_numBlockGroups_OneGroup(t *testing.T) {
	sb := &ext4.Superblock{BlocksCount: 1000, BlocksPerGroup: 8192}
	if got := ext4.NumBlockGroups(sb); got != 1 {
		t.Errorf("one group: got %d, want 1", got)
	}
}

func TestSuperblock_bgdTableBlock_4k(t *testing.T) {
	sb := &ext4.Superblock{BlockSize: 4096}
	if got := ext4.BgdTableBlock(sb); got != 1 {
		t.Errorf("4k block: got %d, want 1", got)
	}
}

func TestSuperblock_bgdTableBlock_2k(t *testing.T) {
	sb := &ext4.Superblock{BlockSize: 2048}
	if got := ext4.BgdTableBlock(sb); got != 1 {
		t.Errorf("2k block: got %d, want 1", got)
	}
}

func TestSuperblock_bgdTableBlock_1k(t *testing.T) {
	sb := &ext4.Superblock{BlockSize: 1024}
	if got := ext4.BgdTableBlock(sb); got != 2 {
		t.Errorf("1k block: got %d, want 2", got)
	}
}

func TestSuperblock_csumSeed_FromUUID(t *testing.T) {
	sb := &ext4.Superblock{FeatureIncompat: 0, UUID: [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}}
	want := ext4.CRC32c(^uint32(0), sb.UUID[:])
	if got := ext4.CsumSeed(sb); got != want {
		t.Errorf("csumSeed from UUID: got 0x%08X, want 0x%08X", got, want)
	}
}

func TestSuperblock_csumSeed_FromField(t *testing.T) {
	sb := &ext4.Superblock{FeatureIncompat: ext4.FeatIncompatCsumSeed, ChecksumSeed: 0xDEADBEEF}
	if got := ext4.CsumSeed(sb); got != 0xDEADBEEF {
		t.Errorf("csumSeed from field: got 0x%08X, want 0xDEADBEEF", got)
	}
}

// ---------------------------------------------------------------------------
// partitionOffset — using in-memory buffers
// ---------------------------------------------------------------------------

func TestPartitionOffset_Bare(t *testing.T) {
	buf := make([]byte, 4096)
	off, err := ext4.PartitionOffset(memReaderAt(buf), -1)
	if err != nil {
		t.Fatalf("bare: unexpected error: %v", err)
	}
	if off != 0 {
		t.Errorf("bare image: got offset %d, want 0", off)
	}
}

func TestPartitionOffset_MBR(t *testing.T) {
	buf := make([]byte, 4096)
	le := binary.LittleEndian
	buf[510] = 0x55
	buf[511] = 0xAA
	e := buf[446:]
	e[4] = 0x83
	le.PutUint32(e[8:], 2048)
	le.PutUint32(e[12:], 0x200000)
	off, err := ext4.PartitionOffset(memReaderAt(buf), -1)
	if err != nil {
		t.Fatalf("MBR: unexpected error: %v", err)
	}
	if want := int64(2048 * 512); off != want {
		t.Errorf("MBR: got offset %d, want %d", off, want)
	}
}

func TestPartitionOffset_MBR_ExplicitIndex(t *testing.T) {
	buf := make([]byte, 4096)
	le := binary.LittleEndian
	buf[510] = 0x55
	buf[511] = 0xAA
	e1 := buf[446+16:]
	e1[4] = 0x83
	le.PutUint32(e1[8:], 4096)
	le.PutUint32(e1[12:], 0x100000)
	off, err := ext4.PartitionOffset(memReaderAt(buf), 1)
	if err != nil {
		t.Fatalf("MBR explicit index 1: unexpected error: %v", err)
	}
	if want := int64(4096 * 512); off != want {
		t.Errorf("MBR explicit index 1: got offset %d, want %d", off, want)
	}
}

func TestPartitionOffset_GPT(t *testing.T) {
	le := binary.LittleEndian
	buf := make([]byte, 64*1024)
	hdr := buf[512:]
	copy(hdr[0:8], "EFI PART")
	le.PutUint64(hdr[72:], 2)
	le.PutUint32(hdr[80:], 1)
	le.PutUint32(hdr[84:], 128)
	entry := buf[1024:]
	copy(entry[0:16], ext4.LinuxPartTypeGPT[:])
	copy(entry[16:32], []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16})
	le.PutUint64(entry[32:], 2048)
	off, err := ext4.PartitionOffset(memReaderAt(buf), -1)
	if err != nil {
		t.Fatalf("GPT: unexpected error: %v", err)
	}
	if want := int64(2048 * 512); off != want {
		t.Errorf("GPT: got offset %d, want %d", off, want)
	}
}

func TestPartitionOffset_GPT_IndexOutOfRange(t *testing.T) {
	le := binary.LittleEndian
	buf := make([]byte, 64*1024)
	hdr := buf[512:]
	copy(hdr[0:8], "EFI PART")
	le.PutUint64(hdr[72:], 2)
	le.PutUint32(hdr[80:], 1)
	le.PutUint32(hdr[84:], 128)
	entry := buf[1024:]
	copy(entry[0:16], ext4.LinuxPartTypeGPT[:])
	le.PutUint64(entry[32:], 2048)
	_, err := ext4.PartitionOffset(memReaderAt(buf), 5)
	if err == nil {
		t.Error("expected error for out-of-range GPT partition index")
	}
}

func TestPartitionOffset_MBR_NoLinuxPartition(t *testing.T) {
	buf := make([]byte, 4096)
	le := binary.LittleEndian
	buf[510] = 0x55
	buf[511] = 0xAA
	e := buf[446:]
	e[4] = 0x0C
	le.PutUint32(e[8:], 2048)
	_, err := ext4.PartitionOffset(memReaderAt(buf), -1)
	if err == nil {
		t.Error("expected error when no Linux partition found in MBR")
	}
}

// ---------------------------------------------------------------------------
// inode helpers
// ---------------------------------------------------------------------------

func TestInode_SetSize(t *testing.T) {
	in := ext4.NewTestInode(10, 256)
	ext4.SetSize(in, 0x1_0000_0001)
	if ext4.InodeSizeVal(in) != 0x1_0000_0001 {
		t.Errorf("SetSize: in.size = %d, want %d", ext4.InodeSizeVal(in), uint64(0x1_0000_0001))
	}
	le := binary.LittleEndian
	raw := ext4.InodeRaw(in)
	lo := uint64(le.Uint32(raw[ext4.InodeOffSizeLo:]))
	hi := uint64(le.Uint32(raw[ext4.InodeOffSizeHi:]))
	if hi<<32|lo != 0x1_0000_0001 {
		t.Errorf("SetSize raw mismatch: hi=0x%X lo=0x%X", hi, lo)
	}
}

func TestInode_SetMode_RegularFile(t *testing.T) {
	in := ext4.NewTestInode(5, 256)
	ext4.SetMode(in, 0x81A4, 1)
	if !ext4.IsRegular(in) {
		t.Error("expected IsRegular() = true")
	}
	if ext4.IsDir(in) {
		t.Error("expected IsDir() = false")
	}
}

func TestInode_SetMode_Directory(t *testing.T) {
	in := ext4.NewTestInode(5, 256)
	ext4.SetMode(in, 0x41ED, 2)
	if !ext4.IsDir(in) {
		t.Error("expected IsDir() = true")
	}
	if ext4.IsRegular(in) {
		t.Error("expected IsRegular() = false")
	}
}

func TestInode_IsSymlink(t *testing.T) {
	in := ext4.NewTestInode(5, 256)
	ext4.SetMode(in, 0xA1FF, 1)
	if !ext4.IsSymlink(in) {
		t.Error("expected IsSymlink() = true")
	}
}

func TestInode_SetInlineExtents_SingleExtent(t *testing.T) {
	in := ext4.NewTestInode(5, 256)
	exts := []ext4.ExtentLeaf{{LogBlock: 0, PhysBlock: 1000, Count: 4}}
	if err := ext4.SetInlineExtents(in, exts); err != nil {
		t.Fatalf("SetInlineExtents: %v", err)
	}
	le := binary.LittleEndian
	ibuf := ext4.InodeRaw(in)[ext4.InodeOffBlock : ext4.InodeOffBlock+60]
	if magic := le.Uint16(ibuf[0:]); magic != ext4.ExtentMagic {
		t.Errorf("magic: got 0x%04X, want 0x%04X", magic, ext4.ExtentMagic)
	}
	if entries := le.Uint16(ibuf[2:]); entries != 1 {
		t.Errorf("entries: got %d, want 1", entries)
	}
	if depth := le.Uint16(ibuf[6:]); depth != 0 {
		t.Errorf("depth: got %d, want 0", depth)
	}
	if lb := le.Uint32(ibuf[12:]); lb != 0 {
		t.Errorf("leaf[0] logBlock: got %d, want 0", lb)
	}
	if cnt := le.Uint16(ibuf[16:]); cnt != 4 {
		t.Errorf("leaf[0] count: got %d, want 4", cnt)
	}
	physLo := uint64(le.Uint32(ibuf[20:]))
	physHi := uint64(le.Uint16(ibuf[18:]))
	if phys := physHi<<32 | physLo; phys != 1000 {
		t.Errorf("leaf[0] physBlock: got %d, want 1000", phys)
	}
}

func TestInode_SetInlineExtents_TooMany(t *testing.T) {
	in := ext4.NewTestInode(5, 256)
	exts := make([]ext4.ExtentLeaf, 5)
	if err := ext4.SetInlineExtents(in, exts); err == nil {
		t.Error("expected error for >4 inline extents")
	}
}

func TestInode_SetInlineExtents_SetsExtentFlag(t *testing.T) {
	in := ext4.NewTestInode(5, 256)
	if err := ext4.SetInlineExtents(in, []ext4.ExtentLeaf{{PhysBlock: 100, Count: 1}}); err != nil {
		t.Fatalf("SetInlineExtents: %v", err)
	}
	raw := ext4.InodeRaw(in)
	flags := binary.LittleEndian.Uint32(raw[ext4.InodeOffFlags:])
	if flags&ext4.InodeFlagExtents == 0 {
		t.Error("ExtentFlag should be set after SetInlineExtents")
	}
}

func TestComputeInodeCsum_DeterministicAndNonZero(t *testing.T) {
	sb := &ext4.Superblock{FeatureIncompat: ext4.FeatIncompatCsumSeed, ChecksumSeed: 0xABCD1234}
	in := ext4.NewTestInode(3, 256)
	inraw := ext4.InodeRaw(in)
	inraw[0] = 0xA4
	inraw[1] = 0x81
	binary.LittleEndian.PutUint32(inraw[ext4.InodeOffGeneration:], 42)
	ext4.ComputeInodeCsum(sb, in)
	lo := binary.LittleEndian.Uint16(inraw[ext4.InodeOffCsumLo:])
	hi := binary.LittleEndian.Uint16(inraw[ext4.InodeOffCsumHi:])
	if lo == 0 && hi == 0 {
		t.Error("inode checksum should not be zero for non-trivial data")
	}
	csum1 := uint32(lo) | uint32(hi)<<16
	ext4.ComputeInodeCsum(sb, in)
	lo2 := binary.LittleEndian.Uint16(inraw[ext4.InodeOffCsumLo:])
	hi2 := binary.LittleEndian.Uint16(inraw[ext4.InodeOffCsumHi:])
	csum2 := uint32(lo2) | uint32(hi2)<<16
	if csum1 != csum2 {
		t.Errorf("inode csum not idempotent: 0x%08X != 0x%08X", csum1, csum2)
	}
}

func TestComputeInodeCsum_DifferentInodes(t *testing.T) {
	sb := &ext4.Superblock{FeatureIncompat: ext4.FeatIncompatCsumSeed, ChecksumSeed: 0x1234}
	in1 := ext4.NewTestInode(3, 256)
	in2 := ext4.NewTestInode(4, 256)
	ext4.ComputeInodeCsum(sb, in1)
	ext4.ComputeInodeCsum(sb, in2)
	lo1 := binary.LittleEndian.Uint16(ext4.InodeRaw(in1)[ext4.InodeOffCsumLo:])
	lo2 := binary.LittleEndian.Uint16(ext4.InodeRaw(in2)[ext4.InodeOffCsumLo:])
	if lo1 == lo2 {
		t.Error("different inode numbers should produce different checksums")
	}
}

// ---------------------------------------------------------------------------
// superblock writeSuperblock round-trip (no real I/O — in-memory buffer)
// ---------------------------------------------------------------------------

func TestSuperblockWriteReadRoundtrip(t *testing.T) {
	mf := &memFile{buf: make([]byte, 64*1024)}
	le := binary.LittleEndian
	raw := make([]byte, 1024)
	le.PutUint16(raw[56:], 0xEF53)
	le.PutUint32(raw[0:], 100)
	le.PutUint32(raw[4:], 8192)
	le.PutUint32(raw[12:], 800)
	le.PutUint32(raw[16:], 90)
	le.PutUint32(raw[20:], 0)
	le.PutUint32(raw[24:], 2)
	le.PutUint32(raw[32:], 8192)
	le.PutUint32(raw[40:], 256)
	le.PutUint16(raw[88:], 256)
	le.PutUint16(raw[0xFE:], 32)
	copy(mf.buf[1024:], raw)
	sb, err := ext4.ReadSuperblock(mf, 0)
	if err != nil {
		t.Fatalf("readSuperblock: %v", err)
	}
	if sb.BlockSize != 4096 {
		t.Errorf("BlockSize: got %d, want 4096", sb.BlockSize)
	}
	if sb.FreeBlocksCount != 800 {
		t.Errorf("FreeBlocksCount: got %d, want 800", sb.FreeBlocksCount)
	}
	sb.FreeBlocksCount = 750
	sb.FreeInodesCount = 80
	if err := ext4.WriteSuperblock(mf, 0, sb); err != nil {
		t.Fatalf("writeSuperblock: %v", err)
	}
	sb2, err := ext4.ReadSuperblock(mf, 0)
	if err != nil {
		t.Fatalf("re-read superblock: %v", err)
	}
	if sb2.FreeBlocksCount != 750 {
		t.Errorf("after write: FreeBlocksCount = %d, want 750", sb2.FreeBlocksCount)
	}
	if sb2.FreeInodesCount != 80 {
		t.Errorf("after write: FreeInodesCount = %d, want 80", sb2.FreeInodesCount)
	}
}

func TestReadWriteRawBlock(t *testing.T) {
	sb := &ext4.Superblock{BlockSize: 512}
	mf := &memFile{buf: make([]byte, 10*512)}
	data := make([]byte, 512)
	for i := range data {
		data[i] = byte(i)
	}
	if err := ext4.WriteRawBlock(mf, 0, sb, 3, data); err != nil {
		t.Fatalf("WriteRawBlock: %v", err)
	}
	got, err := ext4.ReadRawBlock(mf, 0, sb, 3)
	if err != nil {
		t.Fatalf("ReadRawBlock: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Error("readRawBlock did not return data written by writeRawBlock")
	}
}

func TestReadWriteRawBlock_WithOffset(t *testing.T) {
	sb := &ext4.Superblock{BlockSize: 512}
	const fsOffset = int64(1024)
	mf := &memFile{buf: make([]byte, 10*512+2048)}
	data := bytes.Repeat([]byte{0xAB}, 512)
	if err := ext4.WriteRawBlock(mf, fsOffset, sb, 0, data); err != nil {
		t.Fatalf("WriteRawBlock: %v", err)
	}
	got, err := ext4.ReadRawBlock(mf, fsOffset, sb, 0)
	if err != nil {
		t.Fatalf("ReadRawBlock: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Error("readRawBlock with offset did not return written data")
	}
}

func TestBGD_EncodeDecodeRoundtrip_32bit(t *testing.T) {
	le := binary.LittleEndian
	sb := &ext4.Superblock{DescSize: 32, BlockSize: 4096}
	raw := make([]byte, 32)
	le.PutUint32(raw[0:], 100)
	le.PutUint32(raw[4:], 101)
	le.PutUint32(raw[8:], 102)
	le.PutUint16(raw[12:], 500)
	le.PutUint16(raw[14:], 200)
	le.PutUint16(raw[16:], 5)
	d := ext4.DecodeBGD(raw, sb)
	if d.BlockBitmapBlock != 100 {
		t.Errorf("BlockBitmapBlock: got %d, want 100", d.BlockBitmapBlock)
	}
	if d.FreeBlocksCount != 500 {
		t.Errorf("FreeBlocksCount: got %d, want 500", d.FreeBlocksCount)
	}
	d.FreeBlocksCount = 400
	b := ext4.EncodeBGD(d, sb)
	if fbc := le.Uint16(b[12:]); fbc != 400 {
		t.Errorf("after encode: FreeBlocksCount = %d, want 400", fbc)
	}
}

func TestBGD_EncodeDecodeRoundtrip_64bit(t *testing.T) {
	le := binary.LittleEndian
	sb := &ext4.Superblock{DescSize: 64, BlockSize: 4096, FeatureIncompat: ext4.FeatIncompat64bit}
	raw := make([]byte, 64)
	le.PutUint32(raw[0:], 100)
	le.PutUint32(raw[32:], 1)
	le.PutUint16(raw[12:], 500)
	le.PutUint16(raw[44:], 1)
	d := ext4.DecodeBGD(raw, sb)
	wantBitmap := uint64(1)<<32 | 100
	if d.BlockBitmapBlock != wantBitmap {
		t.Errorf("64-bit BlockBitmapBlock: got %d, want %d", d.BlockBitmapBlock, wantBitmap)
	}
	wantFBC := uint32(1)<<16 | 500
	if d.FreeBlocksCount != wantFBC {
		t.Errorf("64-bit FreeBlocksCount: got %d, want %d", d.FreeBlocksCount, wantFBC)
	}
}
