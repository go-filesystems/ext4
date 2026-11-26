package filesystem_ext4

import (
	"encoding/binary"
	"errors"
	"fmt"
	iofs "io/fs"
	"sync/atomic"
	"time"

	"github.com/go-volumes/safeio"
)

var removeDirChildDir func(readerWriterAt, int64, *superblock, string) error
var removeDirChildFile = removeFile
var removeDirFreeBlocks = freeInodeBlocks
var removeDirFreeSlot = freeDirInodeSlot

func init() {
	removeDirChildDir = removeDirTree
}

// DeleteFile removes the regular file at path from the ext4 filesystem.
// The directory entry is zeroed and the inode (with its data blocks) is freed
// when the link count drops to zero. Returns nil if path does not exist
// (idempotent).
func (fs *ext4FS) DeleteFile(path string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return removeFile(fs.f, fs.partOffset, fs.sb, path)
}

// removeFile orchestrates the full file-removal sequence.
func removeFile(f readerWriterAt, fsOffset int64, sb *superblock, path string) error {
	// ⛔ Idempotent is not the same as silent.
	//
	// This used to be a bare `return nil` on any error from either lookup.
	// The comment said "not found"; the code said "anything". A malformed
	// path, or a lookup that failed because the image is broken, came back as
	// a SUCCESSFUL delete -- the caller told the file was gone, nothing
	// touched. Only the missing-path case is the documented one.
	parent, name, err := lookupParent(f, fsOffset, sb, path)
	if err != nil {
		if errors.Is(err, iofs.ErrNotExist) {
			return nil // parent not there — already gone, idempotent
		}
		return err
	}
	target, err := lookupPath(f, fsOffset, sb, path)
	if err != nil {
		if errors.Is(err, iofs.ErrNotExist) {
			return nil // entry not there — already gone, idempotent
		}
		return err
	}
	if !target.isRegular() {
		return fmt.Errorf("ext4: DeleteFile: %q is not a regular file", path)
	}
	if err := zeroDirEntry(f, fsOffset, sb, parent, name); err != nil {
		return err
	}
	return decrementAndFreeInode(f, fsOffset, sb, target)
}

// zeroDirEntry finds the directory entry named name inside dirIno and sets its
// inode field to 0. A zeroed inode marks the slot as free without disturbing
// the surrounding layout.
func zeroDirEntry(f readerWriterAt, fsOffset int64, sb *superblock, dirIno *inode, name string) error {
	exts, err := dirIno.extents(f, fsOffset, sb)
	if err != nil {
		return err
	}
	le := binary.LittleEndian
	for _, e := range exts {
		for blk := uint64(0); blk < uint64(e.Count); blk++ {
			physBlock := e.PhysBlock + blk
			buf, err := readRawBlock(f, fsOffset, sb, physBlock)
			if err != nil {
				return err
			}
			if err := verifyDirBlockCsum(sb, dirIno.num, dirIno.generation(), physBlock, buf); err != nil {
				return err
			}
			if zeroEntryInBlock(buf, name, le) {
				// Trace the directory zeroing (deletion) for debugging.
				traceDirOp("zero", dirIno.num, 0, name)
				updateDirBlockCsum(buf, sb, dirIno)
				return writeRawBlock(f, fsOffset, sb, physBlock, buf)
			}
		}
	}
	return nil // entry not found — idempotent
}

// zeroEntryInBlock scans a directory data block and zeroes the inode of the
// entry matching name. Returns true when an entry was modified.
func zeroEntryInBlock(buf []byte, name string, le binary.ByteOrder) bool {
	off := 0
	for off+8 <= len(buf) {
		recLen := int(le.Uint16(buf[off+4:]))
		if recLen < 8 {
			break
		}
		if buf[off+7] == FtDirTail {
			break
		}
		nameLen := int(buf[off+6])
		if nameLen > 0 && string(buf[off+8:off+8+nameLen]) == name {
			le.PutUint32(buf[off:], 0) // zero inode marks entry as free
			return true
		}
		off += recLen
	}
	return false
}

// decrementAndFreeInode decrements the inode's link count and, when it reaches
// zero, frees the data blocks and the inode slot itself.
func decrementAndFreeInode(f readerWriterAt, fsOffset int64, sb *superblock, in *inode) error {
	// Serialize operations that modify inode metadata and free blocks/slot
	// using a per-inode exclusive lock to avoid exposing transient states
	// to concurrent readers while avoiding serializing all operations on
	// the backing file.
	owner := NewOwner()
	il := getInodeLock(f, in.num)
	il.LockOwner(owner)
	defer il.UnlockOwner(owner)
	le := binary.LittleEndian
	oldLinks := le.Uint16(in.raw[inodeOffLinksCount:])
	newLinks := oldLinks
	if newLinks > 0 {
		newLinks--
	}
	le.PutUint16(in.raw[inodeOffLinksCount:], newLinks)
	// Test-only trace: report the decrement and potential free of the inode.
	traceDecrementAndFreeInode(f, fsOffset, sb, in.num, oldLinks, newLinks)
	if newLinks > 0 {
		return writeInode(f, fsOffset, sb, in)
	}
	if err := freeInodeBlocks(f, fsOffset, sb, in); err != nil {
		return err
	}
	// Leave the inode as the kernel does on eviction (ext4_evict_inode,
	// ext4_truncate, ext4_free_inode): no links, a deletion time, no size, no
	// blocks, an empty extent tree. Without it e2fsck sees an inode that still
	// owns blocks the bitmap says are free.
	scrubFreedInode(in, uint32(time.Now().Unix()))
	if err := writeInode(f, fsOffset, sb, in); err != nil {
		return err
	}
	return freeInodeSlot(f, fsOffset, sb, in.num)
}

// scrubFreedInode clears what a deleted inode no longer owns and stamps i_dtime.
func scrubFreedInode(in *inode, dtime uint32) {
	le := binary.LittleEndian
	le.PutUint16(in.raw[inodeOffLinksCount:], 0)
	le.PutUint32(in.raw[inodeOffDtime:], dtime)
	in.setSize(0)
	in.setBlocks512(0)
	le.PutUint16(in.raw[inodeOffBlocksHi:], 0)
	if in.flags()&InodeFlagExtents != 0 {
		blk := in.raw[inodeOffBlock : inodeOffBlock+60]
		clear(blk)
		le.PutUint16(blk[0:], ExtentMagic)
		le.PutUint16(blk[4:], 4) // eh_max: four inline entries
	}
}

// freeInodeSlot marks inode inodeNum as free in its block group bitmap.
func freeInodeSlot(f readerWriterAt, fsOffset int64, sb *superblock, inodeNum uint32) error {
	return freeInodeSlotKind(f, fsOffset, sb, inodeNum, false)
}

// freeDirInodeSlot is freeInodeSlot for a directory: it also decrements the
// group's bg_used_dirs_count, as ext4_free_inode does for a directory.
func freeDirInodeSlot(f readerWriterAt, fsOffset int64, sb *superblock, inodeNum uint32) error {
	return freeInodeSlotKind(f, fsOffset, sb, inodeNum, true)
}

func freeInodeSlotKind(f readerWriterAt, fsOffset int64, sb *superblock, inodeNum uint32, dir bool) error {
	idx := inodeNum - 1
	g := idx / sb.InodesPerGroup
	bit := int(idx % sb.InodesPerGroup)
	// Acquire locks in canonical order: per-group BGD -> bitmap. Prepare
	// descriptor and bitmap under both locks, release BGD before IO, then
	// write bitmap (while holding bitmap lock) and finally persist the BGD
	// and superblock.
	unlockBGD := lockBGDGroup(f, g)
	unlockBitmap := lockBitmapGroup(f, g, false)

	// Re-read under locks to avoid races.
	d, err := readBGD(f, fsOffset, sb, g)
	if err != nil {
		unlockBitmap()
		unlockBGD()
		return err
	}
	bmap, err := readBitmap(f, fsOffset, sb, d.InodeBitmapBlock)
	if err != nil {
		unlockBitmap()
		unlockBGD()
		return err
	}
	clearBit(bmap, bit)
	d.FreeInodesCount++
	if dir && d.UsedDirsCount > 0 {
		d.UsedDirsCount--
	}

	// Release BGD before performing IO. Keep bitmap locked and write it
	// without re-acquiring the bitmap lock inside writeBitmapBufNoLock.
	unlockBGD()
	if err := writeBitmapBufNoLock(f, fsOffset, sb, g, d, false, d.InodeBitmapBlock, bmap); err != nil {
		unlockBitmap()
		return err
	}
	// release bitmap lock so writeBGD can take per-group descriptor lock
	// in canonical order when persisting the descriptor.
	unlockBitmap()

	if err := writeBGD(f, fsOffset, sb, g, d); err != nil {
		return err
	}
	atomic.AddUint32(&sb.FreeInodesCount, 1)
	traceFreeInodeSlot(f, fsOffset, sb, inodeNum)
	return writeSuperblock(f, fsOffset, sb)
}

// ext4TreeNode is a directory seen by checkDirTree, identified by inode.
type ext4TreeNode struct {
	ino uint32
}

// checkDirTree walks the directory tree under target WITHOUT recursion, before
// removeDirTree changes anything, and refuses a hostile image whose directories
// form a cycle (a directory entry of type directory naming an ancestor: a
// directory hard link, which Link refuses to create but an image can contain),
// a directory reachable twice, or a nesting deeper than safeio.MaxTreeDepth.
// Without it removeDirTree recurses by path for ever while holding the fs lock.
func checkDirTree(f readerWriterAt, fsOffset int64, sb *superblock, target *inode) error {
	return safeio.PostOrder(ext4TreeNode{target.num},
		func(n ext4TreeNode) (uint64, bool) { return uint64(n.ino), true },
		func(n ext4TreeNode) ([]ext4TreeNode, error) {
			in := target
			if n.ino != target.num {
				var err error
				if in, err = readInode(f, fsOffset, sb, n.ino); err != nil {
					return nil, err
				}
			}
			ents, err := readDir(f, fsOffset, sb, in)
			if err != nil {
				return nil, err
			}
			kids := make([]ext4TreeNode, 0, len(ents))
			for _, e := range ents {
				if e.Name == "." || e.Name == ".." {
					continue
				}
				if e.FileType == FtDir {
					kids = append(kids, ext4TreeNode{e.Inode})
				}
			}
			return kids, nil
		},
		func(ext4TreeNode) error { return nil }, 0)
}

// removeDir removes the directory at path and all its contents recursively.
// Returns nil if the path does not exist (idempotent). The tree is checked
// first, so a cyclic image is refused with nothing changed.
func removeDir(f readerWriterAt, fsOffset int64, sb *superblock, path string) error {
	target, err := lookupPath(f, fsOffset, sb, path)
	if err == nil && target.isDir() {
		if err := checkDirTree(f, fsOffset, sb, target); err != nil {
			return fmt.Errorf("ext4: DeleteDir %q: %w", path, err)
		}
	}
	return removeDirTree(f, fsOffset, sb, path)
}

// removeDirTree is the recursive deletion; its recursion is bounded by
// checkDirTree, which removeDir runs first.
func removeDirTree(f readerWriterAt, fsOffset int64, sb *superblock, path string) error {
	// Same shape as removeFile above, and the same correction: only a path
	// that is not there is the documented idempotent case.
	parentIno, name, err := lookupParent(f, fsOffset, sb, path)
	if err != nil {
		if errors.Is(err, iofs.ErrNotExist) {
			return nil // parent not there — already gone, idempotent
		}
		return err
	}
	target, err := lookupPath(f, fsOffset, sb, path)
	if err != nil {
		if errors.Is(err, iofs.ErrNotExist) {
			return nil // already gone, idempotent
		}
		return err
	}
	if !target.isDir() {
		return fmt.Errorf("ext4: DeleteDir: %q is not a directory", path)
	}

	// Recursively remove all children.
	children, err := readDir(f, fsOffset, sb, target)
	if err != nil {
		return err
	}
	for _, e := range children {
		if e.Name == "." || e.Name == ".." {
			continue
		}
		childPath := path + "/" + e.Name
		switch e.FileType {
		case FtDir:
			if err := removeDirChildDir(f, fsOffset, sb, childPath); err != nil {
				return err
			}
		default:
			if err := removeDirChildFile(f, fsOffset, sb, childPath); err != nil {
				return err
			}
		}
	}

	// Zero the directory's entry in the parent.
	if err := zeroDirEntry(f, fsOffset, sb, parentIno, name); err != nil {
		return err
	}

	// Free the directory data blocks and inode slot.
	if err := removeDirFreeBlocks(f, fsOffset, sb, target); err != nil {
		return err
	}
	// Leave the inode as the kernel's rmdir does (no links, a deletion time,
	// no size, no blocks, an empty extent tree) before its slot is released.
	scrubFreedInode(target, uint32(time.Now().Unix()))
	if err := writeInode(f, fsOffset, sb, target); err != nil {
		return err
	}
	if err := removeDirFreeSlot(f, fsOffset, sb, target.num); err != nil {
		return err
	}

	// Decrement parent link count: the ".." from the deleted directory is gone.
	le := binary.LittleEndian
	parentLinks := le.Uint16(parentIno.raw[inodeOffLinksCount:])
	if parentLinks > 0 {
		le.PutUint16(parentIno.raw[inodeOffLinksCount:], parentLinks-1)
	}
	return writeInode(f, fsOffset, sb, parentIno)
}
