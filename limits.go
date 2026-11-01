package filesystem_ext4

import (
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"sync/atomic"

	"github.com/go-volumes/safeio"
)

// ErrTooLarge is returned, wrapped, when an on-disk size or count would make
// the driver allocate or walk more than the image can legitimately hold. It is
// safeio.ErrTooLarge, so errors.Is works against either name.
var ErrTooLarge = safeio.ErrTooLarge

// DefaultMaxReadSize is the largest file ReadFile will materialise in memory
// unless SetMaxReadSize says otherwise: 1 GiB.
//
// A hole costs no bytes on disk, so a sparse file's i_size is bounded by
// nothing in the image; only the caller knows how much memory it is willing to
// spend. Anything larger should be read through OpenFile, which allocates
// nothing proportional to the file.
const DefaultMaxReadSize int64 = 1 << 30

// maxReadLimit is the effective ceiling on a materialised file.
func (sb *superblock) maxReadLimit() int64 {
	if m := atomic.LoadInt64(&sb.maxRead); m > 0 {
		return m
	}
	return DefaultMaxReadSize
}

// declaredBytes returns BlocksCount*BlockSize without overflowing, and false
// when the superblock does not say (synthetic superblocks only) or the product
// does not fit an int64.
func (sb *superblock) declaredBytes() (int64, bool) {
	if sb.BlocksCount == 0 || sb.BlockSize == 0 {
		return 0, false
	}
	if sb.BlocksCount > uint64(math.MaxInt64)/uint64(sb.BlockSize) {
		return math.MaxInt64, true
	}
	return int64(sb.BlocksCount) * int64(sb.BlockSize), true
}

// fileSizeCeiling bounds the i_size of a file about to be materialised: the
// declared filesystem size when known (or fallback otherwise), and never more
// than the read limit.
func (sb *superblock) fileSizeCeiling(fallback int64) int64 {
	max, ok := sb.declaredBytes()
	if !ok {
		max = fallback
	}
	if lim := sb.maxReadLimit(); max > lim || max <= 0 {
		max = lim
	}
	return max
}

// blockBudget is the number of blocks one inode may legitimately map: no more
// than the device holds (when known), no more than the superblock declares,
// and, for synthetic superblocks that declare nothing, no more than the read
// limit. It bounds the work and memory a hostile block map or extent list can
// cost per byte of image.
func (sb *superblock) blockBudget() uint64 {
	bs := uint64(sb.BlockSize)
	if bs == 0 {
		bs = 1024
	}
	budget, known := uint64(0), false
	if sb.BlocksCount != 0 {
		budget, known = sb.BlocksCount, true
	}
	if db := atomic.LoadInt64(&sb.deviceBytes); db > 0 {
		if d := uint64(db) / bs; !known || d < budget {
			budget, known = d, true
		}
	}
	if !known {
		budget = uint64(sb.maxReadLimit()) / bs
	}
	return budget
}

// SetMaxReadSize sets the largest file ReadFile will materialise, in bytes.
// n <= 0 restores DefaultMaxReadSize. A file whose i_size exceeds it is refused
// with ErrTooLarge before any buffer is allocated; use OpenFile to read such a
// file without holding it in memory.
func (fs *ext4FS) SetMaxReadSize(n int64) {
	if n < 0 {
		n = 0
	}
	atomic.StoreInt64(&fs.sb.maxRead, n)
}

// MaxReadSize reports the limit SetMaxReadSize configured (or the default).
func (fs *ext4FS) MaxReadSize() int64 { return fs.sb.maxReadLimit() }

// journalRemaining returns how many bytes of the sidecar journal lie between
// its current read offset and its end.
func journalRemaining(js *os.File) (int64, error) {
	pos, err := js.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	st, err := js.Stat()
	if err != nil {
		return 0, err
	}
	if rem := st.Size() - pos; rem > 0 {
		return rem, nil
	}
	return 0, nil
}

// journalReserveDescs checks that nDesc descriptors (descSize bytes each) fit
// in the rem bytes the journal still holds, and returns what is left after
// them. The comparison is in uint64: nDesc*descSize cannot wrap there, and no
// slice has been sized from nDesc yet.
func journalReserveDescs(rem int64, nDesc uint32) (int64, error) {
	need := uint64(nDesc) * descSize
	if need > uint64(rem) {
		return rem, fmt.Errorf("%d descriptors need %d bytes, %d remain in the journal: %w", nDesc, need, rem, safeio.ErrTooLarge)
	}
	return rem - int64(need), nil
}

// validateExtents refuses an extent list whose leaves overlap, logically or
// physically. A valid ext4 inode maps each logical block at most once and never
// maps two logical blocks onto one physical block, so overlap is corruption --
// and it is also an amplifier: n leaves covering the same blocks make a reader
// walk, read and decode those blocks n times (a directory whose 340 leaves all
// cover the same 128 MiB decodes it 340 times), so the work stops being
// proportional to the image. Leaves of zero length map nothing and are ignored.
//
// The common case, leaves already ascending in both logical and physical
// order, is one pass with no allocation; only a disordered list is sorted (on a
// copy: the caller's order is preserved).
func validateExtents(leaves []extentLeaf, inodeNum uint32) error {
	if len(leaves) < 2 {
		return nil
	}
	if err := checkRuns(leaves, inodeNum, "logical", func(l extentLeaf) uint64 { return uint64(l.LogBlock) }); err != nil {
		return err
	}
	return checkRuns(leaves, inodeNum, "physical", func(l extentLeaf) uint64 { return l.PhysBlock })
}

func checkRuns(leaves []extentLeaf, inodeNum uint32, kind string, start func(extentLeaf) uint64) error {
	ordered := true
	var prevEnd uint64
	have := false
	for _, l := range leaves {
		if l.Count == 0 {
			continue
		}
		s := start(l)
		if have && s < prevEnd {
			ordered = false
			break
		}
		prevEnd, have = s+uint64(l.Count), true
	}
	if ordered {
		return nil
	}
	cp := make([]extentLeaf, 0, len(leaves))
	for _, l := range leaves {
		if l.Count != 0 {
			cp = append(cp, l)
		}
	}
	sort.Slice(cp, func(i, j int) bool { return start(cp[i]) < start(cp[j]) })
	for i := 1; i < len(cp); i++ {
		if start(cp[i]) < start(cp[i-1])+uint64(cp[i-1].Count) {
			return fmt.Errorf("ext4: inode %d has overlapping %s extents at block %d: %w", inodeNum, kind, start(cp[i]), safeio.ErrOutOfBounds)
		}
	}
	return nil
}
