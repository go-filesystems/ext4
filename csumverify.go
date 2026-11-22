package filesystem_ext4

// Metadata checksum VERIFICATION (the write side lives next to each structure).
//
// Reference: the Linux kernel ext4 on-disk format, "Checksums" section,
// https://docs.kernel.org/filesystems/ext4/checksums.html, together with
// fs/ext4/{super,ialloc,balloc,inode,namei}.c (ext4_superblock_csum,
// ext4_group_desc_csum, ext4_inode_csum, ext4_*_bitmap_csum_verify,
// ext4_dirblock_csum_verify, ext4_dx_csum). Every checksum is crc32c
// (Castagnoli, no pre/post inversion -- see crc32c in checksum.go):
//
//	superblock      crc32c(~0, sb[0:0x3FC])                       -> s_checksum
//	group desc      metadata_csum: crc32c(seed, le32 group, desc with the
//	                bg_checksum field zeroed), low 16 bits;
//	                gdt_csum only: crc16(~0, uuid, le32 group, desc up to
//	                bg_checksum, desc after it)
//	inode           crc32c(seed, le32 ino, le32 generation, inode with the
//	                checksum fields zeroed); the high half lives in
//	                i_checksum_hi only when i_extra_isize >= 4
//	directory leaf  crc32c(seed, ino, generation, block up to the START of
//	                the 12-byte fake dirent tail (inode 0, rec_len 12,
//	                name_len 0, file_type 0xDE) that carries the checksum;
//	                checked against a real mke2fs image, not only the doc)
//	htree dx node   crc32c(seed, ino, generation, block up to
//	                count_offset+count*8, dx_tail.reserved, 4 zero bytes)
//	bitmaps         crc32c(seed, bitmap[:n]) stored in the group descriptor
//	                (low 16 bits only when the descriptor is 32 bytes)
//
// where seed is crc32c(~0, uuid) or s_checksum_seed (csumSeed).
//
// These are integrity checks against corruption and truncated writes, not
// authentication: the seed derives from the public UUID, so a forger who
// recomputes the checksum passes. What they buy is that an image altered
// without recomputing them (bit rot, a bad sector, a naive edit) is refused
// instead of being served as genuine -- and, just as important, is never read
// back and re-stamped by a later write, which would launder the corruption.
//
// Extent tree blocks (et_checksum at 12+eh_max*12) are NOT verified yet: the
// writer in this package creates those blocks without the tail, so enforcing
// it would make the driver reject its own output. See the follow-up in the PR.

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Options tune how a filesystem is opened.
type Options struct {
	// IgnoreChecksums disables verification of every metadata checksum on
	// read. It is a FORENSIC opt-out for inspecting a damaged image; it is
	// off by default and must not be used to serve untrusted data.
	IgnoreChecksums bool
}

// ErrChecksum is matched (errors.Is) by every *ChecksumError.
var ErrChecksum = errors.New("ext4: metadata checksum mismatch")

// ChecksumError reports a metadata structure whose stored checksum does not
// match the one computed from its contents. Use errors.As to retrieve it.
type ChecksumError struct {
	Kind     string // "superblock", "group descriptor", "inode", "directory block", "directory index block", "block bitmap", "inode bitmap"
	Inode    uint32 // owning inode, when the structure belongs to one
	Group    uint32 // block group, when the structure belongs to one
	Block    uint64 // physical block, when the structure is a block
	Stored   uint32
	Computed uint32
	Detail   string // set instead of Stored/Computed when the structure has no checksum to compare
}

func (e *ChecksumError) Error() string {
	where := ""
	switch e.Kind {
	case "inode":
		where = fmt.Sprintf(" %d", e.Inode)
	case "group descriptor", "block bitmap", "inode bitmap":
		where = fmt.Sprintf(" of group %d", e.Group)
	case "directory block", "directory index block":
		where = fmt.Sprintf(" (block %d, inode %d)", e.Block, e.Inode)
	}
	if e.Detail != "" {
		return fmt.Sprintf("ext4: %s%s: %s", e.Kind, where, e.Detail)
	}
	return fmt.Sprintf("ext4: %s%s: checksum 0x%08x does not match computed 0x%08x", e.Kind, where, e.Stored, e.Computed)
}

// Is makes errors.Is(err, ErrChecksum) true.
func (e *ChecksumError) Is(target error) bool { return target == ErrChecksum }

// csumEnforced reports whether metadata_csum checksums must be verified.
func (sb *superblock) csumEnforced() bool {
	return sb != nil && sb.FeatureROCompat&FeatROCompatMetadataCsum != 0 && !sb.ignoreCsum
}

// gdtCsumEnforced reports whether the legacy crc16 group descriptor checksum
// (gdt_csum, superseded by metadata_csum) must be verified.
func (sb *superblock) gdtCsumEnforced() bool {
	return sb != nil && sb.FeatureROCompat&FeatROCompatMetadataCsum == 0 &&
		sb.FeatureROCompat&FeatROCompatGdtCsum != 0 && !sb.ignoreCsum
}

// crc16 is the kernel's lib/crc16 (CRC-16/ARC, reflected polynomial 0xA001).
func crc16(crc uint16, data []byte) uint16 {
	for _, b := range data {
		crc ^= uint16(b)
		for i := 0; i < 8; i++ {
			if crc&1 != 0 {
				crc = crc>>1 ^ 0xA001
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}

// bgdChecksum computes bg_checksum for descriptor raw (DescSize bytes) of group
// g, per ext4_group_desc_csum. ok is false when the filesystem has neither
// metadata_csum nor gdt_csum.
func bgdChecksum(sb *superblock, g uint32, raw []byte) (csum uint16, ok bool) {
	var gb [4]byte
	binary.LittleEndian.PutUint32(gb[:], g)
	switch {
	case sb.FeatureROCompat&FeatROCompatMetadataCsum != 0:
		var zero [2]byte
		c := crc32c(sb.csumSeed(), gb[:])
		c = crc32c(c, raw[:30])
		c = crc32c(c, zero[:])
		c = crc32c(c, raw[32:])
		return uint16(c), true
	case sb.FeatureROCompat&FeatROCompatGdtCsum != 0:
		c := crc16(0xFFFF, sb.UUID[:])
		c = crc16(c, gb[:])
		c = crc16(c, raw[:30])
		c = crc16(c, raw[32:])
		return c, true
	}
	return 0, false
}

// stampBGDCsum recomputes bg_checksum in raw (write side of bgdChecksum). It is
// a no-op unless metadata_csum or gdt_csum is set.
func stampBGDCsum(sb *superblock, g uint32, raw []byte) {
	if c, ok := bgdChecksum(sb, g, raw); ok {
		binary.LittleEndian.PutUint16(raw[30:], c)
	}
}

// verifyBGDCsum checks the stored bg_checksum of group g's descriptor.
func verifyBGDCsum(sb *superblock, g uint32, raw []byte) error {
	if !sb.csumEnforced() && !sb.gdtCsumEnforced() {
		return nil
	}
	if len(raw) < 32 {
		return &ChecksumError{Kind: "group descriptor", Group: g, Detail: "descriptor too short"}
	}
	want, _ := bgdChecksum(sb, g, raw)
	if got := binary.LittleEndian.Uint16(raw[30:]); got != want {
		return &ChecksumError{Kind: "group descriptor", Group: g, Stored: uint32(got), Computed: uint32(want)}
	}
	return nil
}

// verifySuperblockCsum checks s_checksum over the first 0x3FC bytes.
func verifySuperblockCsum(sb *superblock) error {
	if !sb.csumEnforced() || len(sb.raw) < superblockSize {
		return nil
	}
	want := crc32c(^uint32(0), sb.raw[:0x3FC])
	if got := binary.LittleEndian.Uint32(sb.raw[0x3FC:]); got != want {
		return &ChecksumError{Kind: "superblock", Stored: got, Computed: want}
	}
	return nil
}

// inodeCsum computes the inode checksum of raw (the full on-disk inode) and
// reports whether the high half (i_checksum_hi) takes part, per
// ext4_inode_csum: the high half exists only when the inode is larger than 128
// bytes and i_extra_isize covers it (>= 4). raw is not modified.
func inodeCsum(sb *superblock, num uint32, raw []byte) (csum uint32, hi bool) {
	le := binary.LittleEndian
	var nb, gb [4]byte
	le.PutUint32(nb[:], num)
	le.PutUint32(gb[:], le.Uint32(raw[100:]))
	hi = len(raw) >= inodeOffCsumHi+2 && le.Uint16(raw[128:]) >= 4
	var zero [2]byte
	c := crc32c(sb.csumSeed(), nb[:])
	c = crc32c(c, gb[:])
	c = crc32c(c, raw[:inodeOffCsumLo])
	c = crc32c(c, zero[:])
	if hi {
		c = crc32c(c, raw[inodeOffCsumLo+2:inodeOffCsumHi])
		c = crc32c(c, zero[:])
		c = crc32c(c, raw[inodeOffCsumHi+2:])
	} else {
		c = crc32c(c, raw[inodeOffCsumLo+2:])
	}
	return c, hi
}

// verifyInodeCsum checks the checksum of the on-disk inode raw. An all-zero
// inode slot (never initialised, e.g. lazy_itable_init) is accepted, as the
// kernel does.
func verifyInodeCsum(sb *superblock, num uint32, raw []byte) error {
	if !sb.csumEnforced() || len(raw) < 128 {
		return nil
	}
	le := binary.LittleEndian
	want, hi := inodeCsum(sb, num, raw)
	got := uint32(le.Uint16(raw[inodeOffCsumLo:]))
	if hi {
		got |= uint32(le.Uint16(raw[inodeOffCsumHi:])) << 16
	} else {
		want &= 0xFFFF
	}
	if got == want {
		return nil
	}
	for _, b := range raw {
		if b != 0 {
			return &ChecksumError{Kind: "inode", Inode: num, Stored: got, Computed: want}
		}
	}
	return nil
}

// verifyBitmapCsum checks a bitmap block against the checksum stored in its
// group descriptor.
func verifyBitmapCsum(sb *superblock, g uint32, d *bgd, isBlockBitmap bool, bmap []byte) error {
	if !sb.csumEnforced() {
		return nil
	}
	// Uninitialised bitmaps are synthesised, never read (ext4_read_*_bitmap).
	const bgInodeUninit, bgBlockUninit = 0x1, 0x2
	if isBlockBitmap && d.Flags&bgBlockUninit != 0 || !isBlockBitmap && d.Flags&bgInodeUninit != 0 {
		return nil
	}
	le := binary.LittleEndian
	n, kind := inodeBitmapCsumLen(sb), "inode bitmap"
	loOff, hiOff := 26, 58
	if isBlockBitmap {
		n, kind = blockBitmapCsumLen(sb), "block bitmap"
		loOff, hiOff = 24, 56
	}
	if n > len(bmap) {
		n = len(bmap)
	}
	want := crc32c(bitmapCsumSeed(sb, g), bmap[:n])
	got := uint32(le.Uint16(d.raw[loOff:]))
	if int(sb.DescSize) >= 64 {
		got |= uint32(le.Uint16(d.raw[hiOff:])) << 16
	} else {
		want &= 0xFFFF
	}
	if got != want {
		return &ChecksumError{Kind: kind, Group: g, Stored: got, Computed: want}
	}
	return nil
}

// hasDirTail reports whether buf ends in the fake dirent tail: reserved_zero1 (le32 0), rec_len 12,
// name_len 0, file_type 0xDE.
func hasDirTail(buf []byte) bool {
	t := len(buf) - 12
	le := binary.LittleEndian
	return t >= 0 && le.Uint32(buf[t:]) == 0 && le.Uint16(buf[t+4:]) == 12 &&
		buf[t+6] == 0 && buf[t+7] == FtDirTail
}

// direntRecLen decodes rec_len the way ext4_rec_len_from_disk does.
func direntRecLen(raw uint16, blockSize int) int {
	if blockSize < 65536 {
		return int(raw)
	}
	if raw == 0 || raw == 65535 {
		return blockSize
	}
	return int(raw&65532) | int(raw&3)<<16
}

// dxCountOffset locates the dx_countlimit of an htree interior block (the root
// block, or a node whose fake dirent spans the block), per get_dx_countlimit.
// It returns 0 when buf is not an htree interior block.
func dxCountOffset(buf []byte) int {
	le := binary.LittleEndian
	bs := len(buf)
	if bs < 40 {
		return 0
	}
	first := direntRecLen(le.Uint16(buf[4:]), bs)
	switch {
	case first == bs:
		return 8
	case first == 12:
		if direntRecLen(le.Uint16(buf[12+4:]), bs) != bs-12 {
			return 0
		}
		// dx_root_info: reserved_zero (le32), hash_version, info_length == 8.
		if le.Uint32(buf[24:]) != 0 || buf[29] != 8 {
			return 0
		}
		return 32
	}
	return 0
}

// verifyDirBlockCsum checks one directory data block (physical block phys) of
// directory inode num/gen: a leaf must end in the checksum tail, an htree
// interior node carries its checksum in the dx_tail after its entries
// (ext4_dirblock_csum_verify / ext4_dx_csum_verify).
func verifyDirBlockCsum(sb *superblock, num, gen uint32, phys uint64, buf []byte) error {
	if !sb.csumEnforced() {
		return nil
	}
	le := binary.LittleEndian
	var nb, gb [4]byte
	le.PutUint32(nb[:], num)
	le.PutUint32(gb[:], gen)
	seed := crc32c(crc32c(sb.csumSeed(), nb[:]), gb[:])

	if hasDirTail(buf) {
		// Up to the start of the tail dirent, per ext4_dirent_csum_verify.
		want := crc32c(seed, buf[:len(buf)-12])
		if got := le.Uint32(buf[len(buf)-4:]); got != want {
			return &ChecksumError{Kind: "directory block", Inode: num, Block: phys, Stored: got, Computed: want}
		}
		return nil
	}
	co := dxCountOffset(buf)
	if co == 0 {
		return &ChecksumError{Kind: "directory block", Inode: num, Block: phys, Detail: "no checksum tail and not an htree node"}
	}
	limit := int(le.Uint16(buf[co:]))
	count := int(le.Uint16(buf[co+2:]))
	tail := co + limit*8
	if tail+8 > len(buf) || count > limit {
		return &ChecksumError{Kind: "directory index block", Inode: num, Block: phys, Detail: "htree limit/count out of range"}
	}
	var zero [4]byte
	want := crc32c(seed, buf[:co+count*8])
	want = crc32c(want, buf[tail:tail+4])
	want = crc32c(want, zero[:])
	if got := le.Uint32(buf[tail+4:]); got != want {
		return &ChecksumError{Kind: "directory index block", Inode: num, Block: phys, Stored: got, Computed: want}
	}
	return nil
}
