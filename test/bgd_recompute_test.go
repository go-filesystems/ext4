package filesystem_ext4_test

import (
	"encoding/binary"
	"testing"

	ext4 "github.com/go-filesystems/ext4"
)

// countFreeBits counts free (zero) bits in the bitmap up to the group's
// block count.
func countFreeBits(b []byte, sb *ext4.Superblock) int {
	max := int(sb.BlocksPerGroup)
	if max > len(b)*8 {
		max = len(b) * 8
	}
	cnt := 0
	for i := 0; i < max; i++ {
		if b[i/8]&(1<<uint(i%8)) == 0 {
			cnt++
		}
	}
	return cnt
}

func TestWriteBGDUpdatesFreeBlocksCount(t *testing.T) {
	fs, cleanup := ext4.NewTempFS(t)
	defer cleanup()

	rw := ext4.CloneFSImage(t, fs)
	sb := ext4.CloneSuperblockFromFS(fs)

	// Use group 0 for simplicity.
	var g uint32 = 0

	d, err := ext4.ReadBGD(rw, 0, sb, g)
	if err != nil {
		t.Fatalf("ReadBGD: %v", err)
	}

	bmp, err := ext4.ReadRawBlock(rw, 0, sb, d.BlockBitmapBlock)
	if err != nil {
		t.Fatalf("ReadRawBlock: %v", err)
	}

	origFree := countFreeBits(bmp, sb)

	// Flip the first free bit to allocated so the free count decreases.
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

	newFree := countFreeBits(bmp, sb)
	if newFree == origFree {
		t.Fatalf("bitmap modification did not change free count: orig=%d new=%d", origFree, newFree)
	}

	// Call WriteBGD which should recompute and update FreeBlocksCount.
	if err := ext4.WriteBGD(rw, 0, sb, g, d); err != nil {
		t.Fatalf("WriteBGD: %v", err)
	}

	d2, err := ext4.ReadBGD(rw, 0, sb, g)
	if err != nil {
		t.Fatalf("ReadBGD after write: %v", err)
	}

	if int(d2.FreeBlocksCount) != newFree {
		t.Fatalf("BGD FreeBlocksCount mismatch: expected=%d got=%d", newFree, d2.FreeBlocksCount)
	}
}

func TestWriteBGDUpdatesFreeBlocksCount_AfterWriteBitmapWithCsum(t *testing.T) {
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

	// Flip the first free bit to allocated.
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

	// Persist bitmap change then update checksum fields using helper.
	if err := ext4.WriteRawBlock(rw, 0, sb, d.BlockBitmapBlock, bmp); err != nil {
		t.Fatalf("WriteRawBlock: %v", err)
	}
	if err := ext4.WriteBitmapWithCsum(rw, 0, sb, g, d, true); err != nil {
		t.Fatalf("WriteBitmapWithCsum: %v", err)
	}

	newFree := countFreeBits(bmp, sb)

	if err := ext4.WriteBGD(rw, 0, sb, g, d); err != nil {
		t.Fatalf("WriteBGD: %v", err)
	}

	d2, err := ext4.ReadBGD(rw, 0, sb, g)
	if err != nil {
		t.Fatalf("ReadBGD after write: %v", err)
	}

	if int(d2.FreeBlocksCount) != newFree {
		t.Fatalf("BGD FreeBlocksCount mismatch after WriteBitmapWithCsum: expected=%d got=%d", newFree, d2.FreeBlocksCount)
	}
}

func TestWriteBGDUpdatesFreeBlocksCount_WrongDescriptor(t *testing.T) {
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

	// Flip a free bit so the authoritative count changes.
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

	newFree := countFreeBits(bmp, sb)

	// Intentionally corrupt descriptor free count before writing.
	d.FreeBlocksCount = 0

	if err := ext4.WriteBGD(rw, 0, sb, g, d); err != nil {
		t.Fatalf("WriteBGD: %v", err)
	}

	d2, err := ext4.ReadBGD(rw, 0, sb, g)
	if err != nil {
		t.Fatalf("ReadBGD after write: %v", err)
	}

	if int(d2.FreeBlocksCount) != newFree {
		t.Fatalf("BGD FreeBlocksCount mismatch when descriptor was wrong: expected=%d got=%d", newFree, d2.FreeBlocksCount)
	}
}

func TestWriteBGDRecomputeSameTask(t *testing.T) {
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

	// Flip the first free bit to allocated so the free count decreases.
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

	// Read raw BGD bytes so we can corrupt the encoded free count and then
	// enqueue both bitmap+BGD in the same commit task.
	raw := make([]byte, int(sb.DescSize))
	if _, rerr := rw.ReadAt(raw, ext4.BgdOffset(sb, g)); rerr != nil {
		t.Fatalf("ReadAt BGD raw: %v", rerr)
	}
	// Corrupt the on-disk free-blocks field (offset 12 for non-64bit descs).
	if int(sb.DescSize) >= 14 {
		raw[12] = 0
		raw[13] = 0
	}

	starts := []int64{int64(d.BlockBitmapBlock) * int64(sb.BlockSize), ext4.BgdOffset(sb, g)}
	datas := [][]byte{bmp, raw}

	ack, _, err := ext4.EnqueueRawCommitOps(rw, 0, sb, g, starts, datas)
	if err != nil {
		t.Fatalf("EnqueueRawCommitOps: %v", err)
	}
	if aerr := <-ack; aerr != nil {
		t.Fatalf("commit ack error: %v", aerr)
	}

	d2, err := ext4.ReadBGD(rw, 0, sb, g)
	if err != nil {
		t.Fatalf("ReadBGD after write: %v", err)
	}

	if int(d2.FreeBlocksCount) != countFreeBits(bmp, sb) {
		t.Fatalf("BGD FreeBlocksCount mismatch in same-task commit: expected=%d got=%d", countFreeBits(bmp, sb), d2.FreeBlocksCount)
	}
}

func TestWriteBGDUpdatesFreeInodesCount(t *testing.T) {
	fs, cleanup := ext4.NewTempFS(t)
	defer cleanup()

	rw := ext4.CloneFSImage(t, fs)
	sb := ext4.CloneSuperblockFromFS(fs)

	var g uint32 = 0

	d, err := ext4.ReadBGD(rw, 0, sb, g)
	if err != nil {
		t.Fatalf("ReadBGD: %v", err)
	}

	ibmp, err := ext4.ReadRawBlock(rw, 0, sb, d.InodeBitmapBlock)
	if err != nil {
		t.Fatalf("ReadRawBlock(inode bitmap): %v", err)
	}

	// Flip the first free inode bit to allocated.
	changed := false
	for i := 0; i < len(ibmp)*8; i++ {
		if i >= int(sb.InodesPerGroup) {
			break
		}
		bi := i / 8
		bit := uint(i % 8)
		if ibmp[bi]&(1<<bit) == 0 {
			ibmp[bi] |= 1 << bit
			changed = true
			break
		}
	}
	if !changed {
		t.Fatalf("no free inode bit found to flip in inode bitmap")
	}

	if err := ext4.WriteRawBlock(rw, 0, sb, d.InodeBitmapBlock, ibmp); err != nil {
		t.Fatalf("WriteRawBlock(inode bitmap): %v", err)
	}

	newFree := 0
	max := int(sb.InodesPerGroup)
	if max > len(ibmp)*8 {
		max = len(ibmp) * 8
	}
	for i := 0; i < max; i++ {
		if ibmp[i/8]&(1<<uint(i%8)) == 0 {
			newFree++
		}
	}

	if err := ext4.WriteBGD(rw, 0, sb, g, d); err != nil {
		t.Fatalf("WriteBGD: %v", err)
	}

	d2, err := ext4.ReadBGD(rw, 0, sb, g)
	if err != nil {
		t.Fatalf("ReadBGD after write: %v", err)
	}

	if int(d2.FreeInodesCount) != newFree {
		t.Fatalf("BGD FreeInodesCount mismatch: expected=%d got=%d", newFree, d2.FreeInodesCount)
	}
}

func TestBGDMetadataCsumRecomputed64(t *testing.T) {
	fs, cleanup := ext4.NewTempFS(t)
	defer cleanup()

	rw := ext4.CloneFSImage(t, fs)
	sb := ext4.CloneSuperblockFromFS(fs)

	sb.FeatureROCompat |= ext4.FeatROCompatMetadataCsum
	sb.DescSize = 64

	var g uint32 = 0

	d, err := ext4.ReadBGD(rw, 0, sb, g)
	if err != nil {
		t.Fatalf("ReadBGD: %v", err)
	}

	// Modify a block bitmap bit so FreeBlocksCount will change and trigger
	// a BGD rewrite including a checksum recompute.
	bmp, err := ext4.ReadRawBlock(rw, 0, sb, d.BlockBitmapBlock)
	if err != nil {
		t.Fatalf("ReadRawBlock:block bitmap: %v", err)
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
		t.Fatalf("no free bit found to flip in block bitmap")
	}
	if err := ext4.WriteRawBlock(rw, 0, sb, d.BlockBitmapBlock, bmp); err != nil {
		t.Fatalf("WriteRawBlock:block bitmap: %v", err)
	}

	if err := ext4.WriteBGD(rw, 0, sb, g, d); err != nil {
		t.Fatalf("WriteBGD: %v", err)
	}

	d2, err := ext4.ReadBGD(rw, 0, sb, g)
	if err != nil {
		t.Fatalf("ReadBGD after write: %v", err)
	}

	raw := ext4.BgdRaw(d2)
	if len(raw) < int(sb.DescSize) {
		t.Fatalf("BGD raw too small: got %d want >= %d", len(raw), sb.DescSize)
	}

	// Compute expected checksum using the same algorithm as writeBGD.
	cpy := make([]byte, sb.DescSize)
	copy(cpy, raw[:sb.DescSize])
	// zero the checksum field at offset 30
	cpy[30] = 0
	cpy[31] = 0
	le := binary.LittleEndian
	gLE := make([]byte, 4)
	le.PutUint32(gLE, uint32(g))
	csum := ext4.CRC32c(ext4.CsumSeed(sb), gLE)
	csum = ext4.CRC32c(csum, cpy[:sb.DescSize])
	want := uint16(csum & 0xFFFF)
	got := le.Uint16(raw[30:])
	if got != want {
		t.Fatalf("BGD checksum mismatch: got=0x%04x want=0x%04x", got, want)
	}
}

func TestMultiGroupBGDRecompute(t *testing.T) {
	// Create a filesystem sized to exceed one block-group so we have at
	// least two groups, with enough blocks in the second group that it has
	// free data blocks to flip after its metadata reservation. The second
	// group is a sparse-super backup group, so it reserves a backup
	// superblock + GDT + reserved-GDT blocks in addition to its bitmaps and
	// inode table; give it a comfortable margin of free blocks.
	size := int64(4096) * int64(32768+2048)
	fs, cleanup := ext4.NewTempFSWithSize(t, size)
	defer cleanup()

	rw := ext4.CloneFSImage(t, fs)
	sb := ext4.CloneSuperblockFromFS(fs)

	// Select group 1 (second group) to ensure non-zero-group layout paths.
	var g uint32 = 1

	d, err := ext4.ReadBGD(rw, 0, sb, g)
	if err != nil {
		t.Fatalf("ReadBGD: %v", err)
	}

	bmp, err := ext4.ReadRawBlock(rw, 0, sb, d.BlockBitmapBlock)
	if err != nil {
		t.Fatalf("ReadRawBlock: %v", err)
	}

	// Flip the first free bit to allocated.
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

	newFree := countFreeBits(bmp, sb)

	if err := ext4.WriteBGD(rw, 0, sb, g, d); err != nil {
		t.Fatalf("WriteBGD: %v", err)
	}

	d2, err := ext4.ReadBGD(rw, 0, sb, g)
	if err != nil {
		t.Fatalf("ReadBGD after write: %v", err)
	}

	if int(d2.FreeBlocksCount) != newFree {
		t.Fatalf("BGD FreeBlocksCount mismatch (multi-group): expected=%d got=%d", newFree, d2.FreeBlocksCount)
	}
}
