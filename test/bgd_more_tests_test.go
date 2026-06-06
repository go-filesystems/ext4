package filesystem_ext4_test

import (
	"encoding/binary"
	"testing"

	ext4 "github.com/go-filesystems/ext4"
)

func TestBGDRecomputeSlowPath_ReadsBitmapFromDisk(t *testing.T) {
	fs, cleanup := ext4.NewTempFS(t)
	defer cleanup()

	rw := ext4.CloneFSImage(t, fs)
	sb := ext4.CloneSuperblockFromFS(fs)

	var g uint32 = 0

	d, err := ext4.ReadBGD(rw, 0, sb, g)
	if err != nil {
		t.Fatalf("ReadBGD: %v", err)
	}

	bmp, err := ext4.ReadRawBlock(rw, 0, sb, d.BlockBitmapBlock)
	if err != nil {
		t.Fatalf("ReadRawBlock: %v", err)
	}

	changed := false
	for i := 0; i < len(bmp)*8; i++ {
		if i >= int(sb.BlocksPerGroup) {
			break
		}
		bi := i / 8
		bit := uint(i % 8)
		if bmp[bi]&(1<<bit) == 0 {
			bmp[bi] |= 1 << bit
			changed = true
			break
		}
	}
	if !changed {
		t.Fatalf("no free bit found to flip in bitmap")
	}

	if err := ext4.WriteRawBlock(rw, 0, sb, d.BlockBitmapBlock, bmp); err != nil {
		t.Fatalf("WriteRawBlock: %v", err)
	}

	// Corrupt descriptor so only recompute can fix it.
	d.FreeBlocksCount = 0

	if err := ext4.WriteBGD(rw, 0, sb, g, d); err != nil {
		t.Fatalf("WriteBGD: %v", err)
	}

	d2, err := ext4.ReadBGD(rw, 0, sb, g)
	if err != nil {
		t.Fatalf("ReadBGD after write: %v", err)
	}

	// compute expected free count
	max := int(sb.BlocksPerGroup)
	if max > len(bmp)*8 {
		max = len(bmp) * 8
	}
	cnt := 0
	for i := 0; i < max; i++ {
		if bmp[i/8]&(1<<uint(i%8)) == 0 {
			cnt++
		}
	}
	if int(d2.FreeBlocksCount) != cnt {
		t.Fatalf("BGD FreeBlocksCount mismatch slow-path: expected=%d got=%d", cnt, d2.FreeBlocksCount)
	}
}

func TestBGDDesc64_64bitFields_Roundtrip(t *testing.T) {
	fs, cleanup := ext4.NewTempFS(t)
	defer cleanup()

	rw := ext4.CloneFSImage(t, fs)
	sb32 := ext4.CloneSuperblockFromFS(fs)

	var g uint32 = 0
	d32, err := ext4.ReadBGD(rw, 0, sb32, g)
	if err != nil {
		t.Fatalf("ReadBGD: %v", err)
	}
	raw32 := ext4.BgdRaw(d32)

	sb64 := ext4.CloneSuperblockFromFS(fs)
	sb64.FeatureIncompat |= ext4.FeatIncompat64bit
	sb64.DescSize = 64

	raw64 := make([]byte, sb64.DescSize)
	copy(raw64, raw32)
	le := binary.LittleEndian
	le.PutUint32(raw64[32:], 0x11111111)
	le.PutUint32(raw64[36:], 0x22222222)
	le.PutUint32(raw64[40:], 0x33333333)

	d64 := ext4.DecodeBGD(raw64, sb64)
	// verify combined fields (basic sanity)
	if d64.BlockBitmapBlock != (uint64(0x11111111)<<32 | uint64(le.Uint32(raw64[0:]))) {
		t.Fatalf("BlockBitmapBlock decode mismatch")
	}

	// change counts and round-trip encode/decode
	d64.FreeBlocksCount = 0x12345678
	d64.FreeInodesCount = 0x23456789
	out := ext4.EncodeBGD(d64, sb64)
	d64b := ext4.DecodeBGD(out, sb64)
	if d64b.FreeBlocksCount != d64.FreeBlocksCount || d64b.FreeInodesCount != d64.FreeInodesCount {
		t.Fatalf("64-bit desc roundtrip counts mismatch")
	}
}

func TestWriteBGD_NoMetadataCsumWhenDisabled(t *testing.T) {
	fs, cleanup := ext4.NewTempFS(t)
	defer cleanup()

	rw := ext4.CloneFSImage(t, fs)
	sb := ext4.CloneSuperblockFromFS(fs)

	// ensure metadata_csum is disabled
	sb.FeatureROCompat &^= ext4.FeatROCompatMetadataCsum

	var g uint32 = 0
	d, err := ext4.ReadBGD(rw, 0, sb, g)
	if err != nil {
		t.Fatalf("ReadBGD: %v", err)
	}
	raw := ext4.BgdRaw(d)
	raw[30] = 0xAA
	raw[31] = 0xBB

	if err := ext4.WriteBGD(rw, 0, sb, g, d); err != nil {
		t.Fatalf("WriteBGD: %v", err)
	}

	d2, err := ext4.ReadBGD(rw, 0, sb, g)
	if err != nil {
		t.Fatalf("ReadBGD after write: %v", err)
	}
	raw2 := ext4.BgdRaw(d2)
	if raw2[30] != 0xAA || raw2[31] != 0xBB {
		t.Fatalf("checksum modified when metadata csum disabled: got=%02x%02x", raw2[30], raw2[31])
	}
}
