package filesystem_ext4

import (
	"encoding/binary"
	"fmt"
	"runtime"
	"time"
)

// bgd holds a decoded block group descriptor.
type bgd struct {
	raw []byte // on-disk bytes (DescSize long)

	BlockBitmapBlock uint64
	InodeBitmapBlock uint64
	InodeTableBlock  uint64
	FreeBlocksCount  uint32
	FreeInodesCount  uint32
	UsedDirsCount    uint32
	Flags            uint16
	ItableUnused     uint32 // bg_itable_unused: trailing inodes of the table that were never used
}

// Block group flags (bg_flags).
const (
	bgInodeUninit  = 0x1 // inode bitmap and table not initialised
	bgBlockUninit  = 0x2 // block bitmap not initialised
	bgItableZeroed = 0x4 // inode table zeroed
)

// tracksUnused reports whether the filesystem keeps bg_itable_unused and the
// uninit flags (uninit_bg, spelled gdt_csum, or metadata_csum).
func (sb *superblock) tracksUnused() bool {
	return sb.FeatureROCompat&(FeatROCompatMetadataCsum|FeatROCompatGdtCsum) != 0
}

// inodeUninit reports whether the group's inode bitmap is to be read as all
// free, whatever the block holds.
func (d *bgd) inodeUninit(sb *superblock) bool {
	return sb.tracksUnused() && d.Flags&bgInodeUninit != 0
}

// synthInodeBitmap is the bitmap of an INODE_UNINIT group: every inode free,
// the padding after the group's last inode set (ext4_read_inode_bitmap).
func synthInodeBitmap(sb *superblock, size int) []byte {
	bmap := make([]byte, size)
	for i := int(sb.InodesPerGroup); i < size*8; i++ {
		setBit(bmap, i)
	}
	return bmap
}

// synthBlockBitmap is the bitmap of a BLOCK_UNINIT group (ext4_init_block_bitmap):
// the backup superblock and descriptors if the group has them, the group's own
// bitmaps and inode table if they lie in it, and the padding past the group.
func synthBlockBitmap(sb *superblock, g uint32, d *bgd, size int) []byte {
	bmap := make([]byte, size)
	start := uint64(sb.FirstDataBlock) + uint64(g)*uint64(sb.BlocksPerGroup)
	mark := func(blk, n uint64) {
		for b := blk; b < blk+n; b++ {
			if b >= start && b < start+uint64(sb.BlocksPerGroup) {
				setBit(bmap, int(b-start))
			}
		}
	}
	if sb.isSparseSuperGroup(g) && len(sb.raw) > 0xD0 {
		bs := uint64(sb.BlockSize)
		gdt := (uint64(sb.numBlockGroups())*uint64(sb.DescSize) + bs - 1) / bs
		reserved := uint64(binary.LittleEndian.Uint16(sb.raw[0xCE:]))
		mark(start, 1+gdt+reserved)
	}
	mark(d.BlockBitmapBlock, 1)
	mark(d.InodeBitmapBlock, 1)
	mark(d.InodeTableBlock, uint64(sb.InodesPerGroup)*uint64(sb.InodeSize)/uint64(sb.BlockSize))
	// Blocks past the end of the filesystem and the bitmap's padding.
	end := uint64(sb.BlocksPerGroup)
	if left := sb.BlocksCount - start; start+end > sb.BlocksCount {
		end = left
	}
	for i := int(end); i < size*8; i++ {
		setBit(bmap, i)
	}
	return bmap
}

// noteBlocksAllocated records that blocks were taken from the group: its
// bitmap, synthesised until now, is about to be written, so BLOCK_UNINIT goes.
func (d *bgd) noteBlocksAllocated(sb *superblock) {
	if sb.tracksUnused() {
		d.Flags &^= bgBlockUninit
	}
}

// noteInodeAllocated records that inode bit (0-based within the group) is now
// in use: the group's inode bitmap is initialised, and no inode at or below it
// is "unused" any more (ext4_new_inode, fs/ext4/ialloc.c). Freeing an inode
// leaves bg_itable_unused alone, as the kernel does.
func (d *bgd) noteInodeAllocated(sb *superblock, bit int) {
	if !sb.tracksUnused() {
		return
	}
	d.Flags &^= bgInodeUninit
	if bit < 0 || uint32(bit) >= sb.InodesPerGroup {
		return
	}
	if free := sb.InodesPerGroup - (uint32(bit) + 1); free < d.ItableUnused {
		d.ItableUnused = free
	}
}

// readBGD reads and decodes the block group descriptor for group g.
func readBGD(f readerWriterAt, fsOffset int64, sb *superblock, g uint32) (*bgd, error) {
	tableBlock := sb.bgdTableBlock()
	descOff := int64(tableBlock)*int64(sb.BlockSize) + int64(g)*int64(sb.DescSize)
	raw := make([]byte, sb.DescSize)
	if _, err := f.ReadAt(raw, fsOffset+descOff); err != nil {
		return nil, fmt.Errorf("ext4: read BGD %d: %w", g, err)
	}
	if err := verifyBGDCsum(sb, g, raw); err != nil {
		return nil, err
	}
	d := decodeBGD(raw, sb)
	if ext4LockDebug {
		_, file, line, _ := runtime.Caller(1)
		debugPrintf("DEBUG ReadBGD g=%d FreeBlocksCount=%d caller=%s:%d\n", g, d.FreeBlocksCount, file, line)
	}
	return d, nil
}

func decodeBGD(raw []byte, sb *superblock) *bgd {
	le := binary.LittleEndian
	d := &bgd{raw: raw}

	blo := uint64(le.Uint32(raw[0:]))
	ilo := uint64(le.Uint32(raw[4:]))
	tlo := uint64(le.Uint32(raw[8:]))

	if sb.FeatureIncompat&FeatIncompat64bit != 0 && sb.DescSize >= 64 {
		bhi := uint64(le.Uint32(raw[32:]))
		ihi := uint64(le.Uint32(raw[36:]))
		thi := uint64(le.Uint32(raw[40:]))
		d.BlockBitmapBlock = (bhi << 32) | blo
		d.InodeBitmapBlock = (ihi << 32) | ilo
		d.InodeTableBlock = (thi << 32) | tlo
		d.FreeBlocksCount = uint32(le.Uint16(raw[12:])) | (uint32(le.Uint16(raw[44:])) << 16)
		d.FreeInodesCount = uint32(le.Uint16(raw[14:])) | (uint32(le.Uint16(raw[46:])) << 16)
		d.UsedDirsCount = uint32(le.Uint16(raw[16:])) | (uint32(le.Uint16(raw[48:])) << 16)
	} else {
		d.BlockBitmapBlock = blo
		d.InodeBitmapBlock = ilo
		d.InodeTableBlock = tlo
		d.FreeBlocksCount = uint32(le.Uint16(raw[12:]))
		d.FreeInodesCount = uint32(le.Uint16(raw[14:]))
		d.UsedDirsCount = uint32(le.Uint16(raw[16:]))
	}
	d.Flags = le.Uint16(raw[18:])
	if len(raw) >= 30 {
		d.ItableUnused = uint32(le.Uint16(raw[28:]))
		if sb.FeatureIncompat&FeatIncompat64bit != 0 && sb.DescSize >= 64 && len(raw) >= 52 {
			d.ItableUnused |= uint32(le.Uint16(raw[50:])) << 16
		}
	}
	return d
}

// encode writes updated free/used counts back into d.raw.
func (d *bgd) encode(sb *superblock) {
	le := binary.LittleEndian
	raw := d.raw

	if sb.FeatureIncompat&FeatIncompat64bit != 0 && sb.DescSize >= 64 {
		le.PutUint16(raw[12:], uint16(d.FreeBlocksCount&0xFFFF))
		le.PutUint16(raw[44:], uint16(d.FreeBlocksCount>>16))
		le.PutUint16(raw[14:], uint16(d.FreeInodesCount&0xFFFF))
		le.PutUint16(raw[46:], uint16(d.FreeInodesCount>>16))
		le.PutUint16(raw[16:], uint16(d.UsedDirsCount&0xFFFF))
		le.PutUint16(raw[48:], uint16(d.UsedDirsCount>>16))
	} else {
		le.PutUint16(raw[12:], uint16(d.FreeBlocksCount))
		le.PutUint16(raw[14:], uint16(d.FreeInodesCount))
		le.PutUint16(raw[16:], uint16(d.UsedDirsCount))
	}
	if len(raw) >= 20 {
		le.PutUint16(raw[18:], d.Flags)
	}
	if len(raw) >= 30 {
		le.PutUint16(raw[28:], uint16(d.ItableUnused))
		if sb.FeatureIncompat&FeatIncompat64bit != 0 && sb.DescSize >= 64 && len(raw) >= 52 {
			le.PutUint16(raw[50:], uint16(d.ItableUnused>>16))
		}
	}
}

// writeBGD writes the updated block group descriptor back to the image and
// refreshes its checksum when metadata_csum is enabled.
func writeBGD(f readerWriterAt, fsOffset int64, sb *superblock, g uint32, d *bgd) error {
	d.encode(sb)

	stampBGDCsum(sb, g, d.raw)

	// Serialize preparation of the descriptor update so concurrent
	// writers don't compute overlapping descriptor bytes. Take the per-
	// group descriptor lock while preparing the on-disk bytes, then
	// release it before performing the actual IO to avoid holding a
	// global descriptor lock during potentially long journal fsyncs.
	unlock := lockBGDGroup(f, g)
	tableBlock := sb.bgdTableBlock()
	descOff := int64(tableBlock)*int64(sb.BlockSize) + int64(g)*int64(sb.DescSize)
	// Make a copy of the prepared raw bytes and enqueue them to the
	// commit dispatcher while still holding the BGD lock so ordering is
	// preserved. The worker will apply direct writes (no per-op
	// transactions) to avoid creating isolated journal commits.
	rawCopy := make([]byte, len(d.raw))
	copy(rawCopy, d.raw)
	ops := []commitOp{{startAbs: fsOffset + descOff, data: rawCopy}}
	ack, seq, err := enqueueCommitWritesUnderLock(f, fsOffset, sb, g, ops)
	if err != nil {
		unlock()
		return fmt.Errorf("ext4: write BGD %d: %w", g, err)
	}
	// Release BGD lock after enqueueing to preserve ordering and avoid
	// holding descriptor locks during IO.
	unlock()
	if ext4LockDebug {
		_, file, line, _ := runtime.Caller(0)
		debugPrintf("DEBUG writeBGD enqueued group=%d seq=%d FreeBlocksCount=%d ts=%d caller=%s:%d\n", g, seq, d.FreeBlocksCount, time.Now().UnixNano(), file, line)
	}
	aerr := <-ack
	if ext4LockDebug {
		_, file, line, _ := runtime.Caller(0)
		debugPrintf("DEBUG ack received writeBGD group=%d seq=%d FreeBlocksCount=%d ack=%p err=%v caller=%s:%d\n", g, seq, d.FreeBlocksCount, &ack, aerr, file, line)
	}
	if aerr != nil {
		return fmt.Errorf("ext4: write BGD %d: %w", g, aerr)
	}
	return nil
}
