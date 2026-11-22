package filesystem_ext4

import (
	"encoding/binary"
	"strings"
	"testing"
)

// Unit tests that pin both sides of each guard in csumverify.go (found by
// mutsweep: a guard nothing asserts on is a guard a refactor can delete).

func csumTestSB() *superblock {
	return &superblock{
		FeatureROCompat: FeatROCompatMetadataCsum,
		DescSize:        64,
		BlockSize:       1024,
		BlocksPerGroup:  8192,
		InodesPerGroup:  2048,
	}
}

func TestChecksumErrorText(t *testing.T) {
	cases := []struct {
		e    ChecksumError
		want string
	}{
		{ChecksumError{Kind: "inode", Inode: 7, Stored: 1, Computed: 2}, "ext4: inode 7: checksum 0x00000001 does not match computed 0x00000002"},
		{ChecksumError{Kind: "group descriptor", Group: 3, Detail: "descriptor too short"}, "ext4: group descriptor of group 3: descriptor too short"},
		{ChecksumError{Kind: "block bitmap", Group: 4, Stored: 5, Computed: 6}, "ext4: block bitmap of group 4: checksum 0x00000005 does not match computed 0x00000006"},
		{ChecksumError{Kind: "directory block", Block: 9, Inode: 2, Detail: "x"}, "ext4: directory block (block 9, inode 2): x"},
		{ChecksumError{Kind: "superblock", Stored: 1, Computed: 2}, "ext4: superblock: checksum 0x00000001 does not match computed 0x00000002"},
	}
	for _, c := range cases {
		if got := c.e.Error(); got != c.want {
			t.Errorf("Error() = %q, want %q", got, c.want)
		}
	}
}

func TestVerifyBitmapCsumGuards(t *testing.T) {
	sb := csumTestSB()
	bmap := make([]byte, 1024)
	bmap[3] = 0x55

	d := &bgd{raw: make([]byte, 64)}
	setBitmapCsum(sb, 2, d, true, bmap)
	setBitmapCsum(sb, 2, d, false, bmap)
	if err := verifyBitmapCsum(sb, 2, d, true, bmap); err != nil {
		t.Fatalf("stamped block bitmap: %v", err)
	}
	if err := verifyBitmapCsum(sb, 2, d, false, bmap); err != nil {
		t.Fatalf("stamped inode bitmap: %v", err)
	}
	bad := append([]byte(nil), bmap...)
	bad[0] ^= 1
	wantChecksumError(t, verifyBitmapCsum(sb, 2, d, true, bad), "block bitmap")
	wantChecksumError(t, verifyBitmapCsum(sb, 2, d, false, bad), "inode bitmap")

	// Ignored: a mismatch is accepted.
	ign := *sb
	ign.ignoreCsum = true
	if err := verifyBitmapCsum(&ign, 2, d, true, bad); err != nil {
		t.Fatalf("ignoreCsum: %v", err)
	}
	// Feature absent: nothing to verify.
	off := *sb
	off.FeatureROCompat = 0
	if err := verifyBitmapCsum(&off, 2, d, true, bad); err != nil {
		t.Fatalf("no metadata_csum: %v", err)
	}

	// BLOCK_UNINIT (0x2) exempts only the block bitmap, INODE_UNINIT (0x1)
	// only the inode bitmap.
	d.Flags = 0x2
	if err := verifyBitmapCsum(sb, 2, d, true, bad); err != nil {
		t.Fatalf("block uninit: %v", err)
	}
	wantChecksumError(t, verifyBitmapCsum(sb, 2, d, false, bad), "inode bitmap")
	d.Flags = 0x1
	if err := verifyBitmapCsum(sb, 2, d, false, bad); err != nil {
		t.Fatalf("inode uninit: %v", err)
	}
	wantChecksumError(t, verifyBitmapCsum(sb, 2, d, true, bad), "block bitmap")
}

func TestDirentRecLen(t *testing.T) {
	cases := []struct {
		raw  uint16
		bs   int
		want int
	}{
		{0, 4096, 0},
		{65535, 4096, 65535},
		{1024, 1024, 1024},
		{0, 65536, 65536},
		{65535, 65536, 65536},
		{4, 65536, 4},
		{5, 65536, 65540},
	}
	for _, c := range cases {
		if got := direntRecLen(c.raw, c.bs); got != c.want {
			t.Errorf("direntRecLen(%d, %d) = %d, want %d", c.raw, c.bs, got, c.want)
		}
	}
}

// htreeRoot builds a 1 KiB htree root block with the given limit/count and no
// valid checksum yet.
func htreeRoot(limit, count uint16) []byte {
	le := binary.LittleEndian
	buf := make([]byte, 1024)
	le.PutUint32(buf[0:], 2)
	le.PutUint16(buf[4:], 12)
	le.PutUint16(buf[16:], 1024-12)
	buf[29] = 8
	le.PutUint16(buf[32:], limit)
	le.PutUint16(buf[34:], count)
	return buf
}

func TestDxCountOffset(t *testing.T) {
	le := binary.LittleEndian
	if got := dxCountOffset(htreeRoot(2, 1)); got != 32 {
		t.Errorf("root: %d, want 32", got)
	}
	b := htreeRoot(2, 1)
	le.PutUint16(b[16:], 1000) // second dirent does not span the block
	if got := dxCountOffset(b); got != 0 {
		t.Errorf("short second dirent: %d, want 0", got)
	}
	b = htreeRoot(2, 1)
	b[24] = 1 // reserved_zero
	if got := dxCountOffset(b); got != 0 {
		t.Errorf("reserved_zero set: %d, want 0", got)
	}
	b = htreeRoot(2, 1)
	b[29] = 7 // info_length
	if got := dxCountOffset(b); got != 0 {
		t.Errorf("info_length 7: %d, want 0", got)
	}
	// A node whose fake dirent spans the whole block.
	node := make([]byte, 1024)
	le.PutUint16(node[4:], 1024)
	if got := dxCountOffset(node); got != 8 {
		t.Errorf("interior node: %d, want 8", got)
	}
	// Smaller than 40 bytes is never an htree block, whatever it claims.
	small := make([]byte, 39)
	le.PutUint16(small[4:], 39)
	if got := dxCountOffset(small); got != 0 {
		t.Errorf("39 bytes: %d, want 0", got)
	}
	small = make([]byte, 40)
	le.PutUint16(small[4:], 40)
	if got := dxCountOffset(small); got != 8 {
		t.Errorf("40 bytes: %d, want 8", got)
	}
}

func stampHtreeRoot(sb *superblock, num, gen uint32, buf []byte) {
	le := binary.LittleEndian
	var nb, gb, zero [4]byte
	le.PutUint32(nb[:], num)
	le.PutUint32(gb[:], gen)
	seed := crc32c(crc32c(sb.csumSeed(), nb[:]), gb[:])
	limit := int(le.Uint16(buf[32:]))
	count := int(le.Uint16(buf[34:]))
	tail := 32 + limit*8
	c := crc32c(seed, buf[:32+count*8])
	c = crc32c(c, buf[tail:tail+4])
	c = crc32c(c, zero[:])
	le.PutUint32(buf[tail+4:], c)
}

func TestVerifyDirBlockCsumHtree(t *testing.T) {
	sb := csumTestSB()
	const limit = (1024 - 32 - 8) / 8 // the tail ends exactly at the block end

	good := htreeRoot(limit, 2)
	stampHtreeRoot(sb, 2, 0, good)
	if err := verifyDirBlockCsum(sb, 2, 0, 5, good); err != nil {
		t.Fatalf("valid htree root: %v", err)
	}

	flipped := append([]byte(nil), good...)
	flipped[len(flipped)-1] ^= 0xFF
	err := verifyDirBlockCsum(sb, 2, 0, 5, flipped)
	wantChecksumError(t, err, "directory index block")
	if ce := err.(*ChecksumError); ce.Detail != "" || ce.Stored == ce.Computed {
		t.Fatalf("want a stored/computed mismatch, got %+v", ce)
	}

	// count > limit and a tail past the block end are refused with a detail.
	over := htreeRoot(5, 6)
	for name, b := range map[string][]byte{"count>limit": over, "tail past end": htreeRoot(limit+1, 1)} {
		err := verifyDirBlockCsum(sb, 2, 0, 5, b)
		wantChecksumError(t, err, "directory index block")
		if !strings.Contains(err.Error(), "out of range") {
			t.Errorf("%s: %v", name, err)
		}
	}

	// Neither a tail nor an htree node.
	plain := make([]byte, 1024)
	err = verifyDirBlockCsum(sb, 2, 0, 5, plain)
	wantChecksumError(t, err, "directory block")
	if !strings.Contains(err.Error(), "not an htree node") {
		t.Errorf("plain block: %v", err)
	}

	// Ignored or feature absent.
	ign := *sb
	ign.ignoreCsum = true
	if err := verifyDirBlockCsum(&ign, 2, 0, 5, plain); err != nil {
		t.Fatalf("ignoreCsum: %v", err)
	}
	off := *sb
	off.FeatureROCompat = 0
	if err := verifyDirBlockCsum(&off, 2, 0, 5, plain); err != nil {
		t.Fatalf("no metadata_csum: %v", err)
	}
}

func TestVerifyBGDCsumShortDescriptor(t *testing.T) {
	sb := csumTestSB()
	err := verifyBGDCsum(sb, 1, make([]byte, 31))
	wantChecksumError(t, err, "group descriptor")
	if !strings.Contains(err.Error(), "too short") {
		t.Errorf("short descriptor: %v", err)
	}
	// 32 bytes is long enough to be compared (and, being zero, is a mismatch).
	err = verifyBGDCsum(sb, 1, make([]byte, 32))
	wantChecksumError(t, err, "group descriptor")
	if strings.Contains(err.Error(), "too short") {
		t.Errorf("32-byte descriptor wrongly short: %v", err)
	}
}
