package filesystem_ext4

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/go-volumes/safeio"
)

// blockMapExtents synthesises an extent list equivalent to the file's indirect
// block map, coalescing logically- and physically-contiguous runs. Holes
// (pointer 0) are omitted, so the LogBlock of each leaf is meaningful. This
// lets directory traversal and other extent() consumers work transparently on
// ext2/ext3 inodes.
//
// The 60-byte i_block area holds 15 little-endian uint32 block pointers:
//
//	[0..11]  direct blocks
//	[12]     single-indirect block (holds BlockSize/4 pointers)
//	[13]     double-indirect block (pointers to single-indirect blocks)
//	[14]     triple-indirect block (pointers to double-indirect blocks)
//
// The map is walked as a tree, not probed one logical block at a time: an
// all-zero pointer skips the whole subtree it would cover, so a sparse file
// declaring a huge i_size costs nothing for its holes. What remains is bounded
// by what the image can hold: every non-zero pointer, to data or to an indirect
// block, is charged against sb.blockBudget() (the blocks the device has), and
// an indirect block reached twice is refused. A real filesystem never maps one
// block twice, but a hostile image can point a double-indirect block at one
// single-indirect block 1024 times, turning three blocks on disk into a million
// leaves in memory.
func (in *inode) blockMapExtents(f readerWriterAt, fsOffset int64, sb *superblock) ([]extentLeaf, error) {
	bs := uint64(sb.BlockSize)
	if bs < 4 {
		return nil, fmt.Errorf("ext4: blockMap: invalid block size %d", sb.BlockSize)
	}
	if in.size == 0 {
		return nil, nil
	}
	nblocks := in.size / bs
	if in.size%bs != 0 {
		nblocks++
	}
	ptrs := bs / 4
	le := binary.LittleEndian
	iblock := in.raw[inodeOffBlock : inodeOffBlock+60]

	budget := sb.blockBudget()
	var used uint64
	var visited safeio.VisitSet
	var out []extentLeaf

	charge := func() error {
		used++
		if used > budget {
			return fmt.Errorf("ext4: inode %d block map names more than the %d blocks the device holds: %w", in.num, budget, safeio.ErrTooLarge)
		}
		return nil
	}
	emit := func(lbn, phys uint64) error {
		if err := charge(); err != nil {
			return err
		}
		if lbn > math.MaxUint32 {
			return fmt.Errorf("ext4: inode %d block map reaches logical block %d: %w", in.num, lbn, safeio.ErrOutOfBounds)
		}
		if n := len(out); n > 0 {
			last := &out[n-1]
			if last.Count < 0xFFFF &&
				last.PhysBlock+uint64(last.Count) == phys &&
				uint64(last.LogBlock)+uint64(last.Count) == lbn {
				last.Count++
				return nil
			}
		}
		out = append(out, extentLeaf{LogBlock: uint32(lbn), PhysBlock: phys, Count: 1})
		return nil
	}

	// span[l] is the number of logical blocks one pointer at level l covers:
	// 1 for a pointer to data, ptrs for a single-indirect block, and so on.
	span := [4]uint64{1, ptrs, ptrs * ptrs, ptrs * ptrs * ptrs}
	var walk func(level int, blk, base uint64) error
	walk = func(level int, blk, base uint64) error {
		if err := charge(); err != nil {
			return err
		}
		if err := visited.Check(blk); err != nil {
			return fmt.Errorf("ext4: inode %d block map reuses indirect block %d: %w", in.num, blk, err)
		}
		buf, err := readRawBlock(f, fsOffset, sb, blk)
		if err != nil {
			return err
		}
		for i := uint64(0); i < ptrs; i++ {
			lbn := base + i*span[level-1]
			if lbn >= nblocks {
				break
			}
			p := uint64(le.Uint32(buf[i*4:]))
			if p == 0 {
				continue
			}
			if level == 1 {
				if err := emit(lbn, p); err != nil {
					return err
				}
			} else if err := walk(level-1, p, lbn); err != nil {
				return err
			}
		}
		return nil
	}

	for i := uint64(0); i < 12 && i < nblocks; i++ {
		if p := uint64(le.Uint32(iblock[i*4:])); p != 0 {
			if err := emit(i, p); err != nil {
				return nil, err
			}
		}
	}
	base := uint64(12)
	for level := 1; level <= 3; level++ {
		if base >= nblocks {
			break
		}
		if p := uint64(le.Uint32(iblock[(11+level)*4:])); p != 0 {
			if err := walk(level, p, base); err != nil {
				return nil, err
			}
		}
		base += span[level]
	}
	return out, nil
}

// blockMapData reads the full content of a block-map file, honouring sparse
// holes (which read back as zeroes). The result is exactly in.size bytes, and
// in.size is checked against the ceiling BEFORE the buffer is allocated.
func (in *inode) blockMapData(f readerWriterAt, fsOffset int64, sb *superblock) ([]byte, error) {
	max := sb.fileSizeCeiling(0)
	out, err := safeio.MakeBytes(clampInt64(in.size), max)
	if err != nil {
		return nil, fmt.Errorf("ext4: inode %d size %d invalid: %w", in.num, in.size, err)
	}
	if len(out) == 0 {
		return out, nil
	}
	leaves, err := in.blockMapExtents(f, fsOffset, sb)
	if err != nil {
		return nil, err
	}
	return out, in.copyLeaves(f, fsOffset, sb, leaves, out)
}

// clampInt64 converts an on-disk uint64 size to int64, saturating instead of
// going negative, so a size >= 2^63 is compared against a ceiling as the huge
// number it is rather than as a negative one.
func clampInt64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}

// copyLeaves reads each leaf's blocks into out at the leaf's logical offset.
// Blocks past len(out) are not read. Holes stay as the zeroes out started with.
func (in *inode) copyLeaves(f readerWriterAt, fsOffset int64, sb *superblock, leaves []extentLeaf, out []byte) error {
	bs := uint64(sb.BlockSize)
	size := uint64(len(out))
	for _, e := range leaves {
		logOff := uint64(e.LogBlock) * bs
		if logOff >= size {
			continue
		}
		n := uint64(e.Count) * bs
		if logOff+n > size {
			n = size - logOff
		}
		if _, err := f.ReadAt(out[logOff:logOff+n], fsOffset+int64(e.PhysBlock*bs)); err != nil {
			return fmt.Errorf("ext4: read data block %d: %w", e.PhysBlock, err)
		}
	}
	return nil
}
