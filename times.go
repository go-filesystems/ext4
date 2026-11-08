package filesystem_ext4

import (
	"encoding/binary"
	"fmt"
	iofs "io/fs"
	"time"

	filesystem "github.com/go-filesystems/interface"
)

var _ filesystem.TimesReader = (*ext4FS)(nil)

// Offsets of the extended timestamp fields in struct ext4_inode (Linux
// fs/ext4/ext4.h): the base 32-bit seconds sit in the 128-byte original inode,
// the "_extra" words and i_crtime in the part that i_extra_isize announces.
const (
	inodeOffCTimeExtra  = 132 // i_ctime_extra  (nsec << 2 | epoch)
	inodeOffMTimeExtra  = 136 // i_mtime_extra
	inodeOffATimeExtra  = 140 // i_atime_extra
	inodeOffCrtime      = 144 // i_crtime
	inodeOffCrtimeExtra = 148 // i_crtime_extra
	inodeGoodOldSize    = 128 // EXT4_GOOD_OLD_INODE_SIZE
)

// Times returns the four times recorded in the inode of path. A symbolic link
// is not followed: its own inode is read.
//
//   - Atime, Mtime, Ctime are the inode's 32-bit signed seconds. When the
//     inode is large enough to hold the field's _extra word (the kernel's
//     EXT4_FITS_IN_INODE: 128 + i_extra_isize reaches its end, and the buffer
//     holds it), the word adds nanoseconds (bits 2..31) and two epoch bits that
//     extend the seconds by 0, 2^32, 2*2^32 or 3*2^32 (through 2446), and the
//     resolution is 1 ns. Otherwise the time is whole seconds, resolution 1 s.
//   - Btime is i_crtime (+ i_crtime_extra), and exists only when the inode is
//     large enough for i_crtime; an inode without the field, or with an
//     all-zero crtime (never set), is reported as not recorded. ext2/ext3
//     images with 128-byte inodes therefore have no Btime.
//   - An _extra word whose nanosecond field is 10^9 or more cannot be a time
//     (a corrupt inode): that time is reported as not recorded.
func (fs *ext4FS) Times(path string) (filesystem.FileTimes, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	rw := getRW(fs)
	var in *inode
	var err error
	if path == "/" {
		in, err = readInode(rw, fs.partOffset, fs.sb, RootIno)
		if err != nil {
			return filesystem.FileTimes{}, err
		}
		return decodeInodeTimes(in.raw), nil
	}
	parent, name, err := lookupParent(rw, fs.partOffset, fs.sb, path)
	if err != nil {
		return filesystem.FileTimes{}, err
	}
	entries, err := readDir(rw, fs.partOffset, fs.sb, parent)
	if err != nil {
		return filesystem.FileTimes{}, err
	}
	for _, e := range entries {
		if e.Name != name {
			continue
		}
		in, err = readInode(rw, fs.partOffset, fs.sb, e.Inode)
		if err != nil {
			return filesystem.FileTimes{}, err
		}
		return decodeInodeTimes(in.raw), nil
	}
	return filesystem.FileTimes{}, fmt.Errorf("ext4: %q not found: %w", name, iofs.ErrNotExist)
}

// decodeInodeTimes decodes the timestamp fields of a raw inode.
func decodeInodeTimes(raw []byte) filesystem.FileTimes {
	le := binary.LittleEndian
	if len(raw) < inodeGoodOldSize {
		return filesystem.FileTimes{}
	}
	// limit is the first byte NOT part of the inode's announced fields.
	limit := inodeGoodOldSize
	if len(raw) >= inodeOffExtraIsize+2 {
		limit += int(le.Uint16(raw[inodeOffExtraIsize:]))
	}
	if limit > len(raw) {
		limit = len(raw)
	}
	fits := func(off int) bool { return off+4 <= limit }
	xtime := func(base, extra int) filesystem.Timestamp {
		sec := int64(int32(le.Uint32(raw[base:])))
		if !fits(extra) {
			return filesystem.NewTimestamp(time.Unix(sec, 0), time.Second)
		}
		x := le.Uint32(raw[extra:])
		sec += int64(x&3) << 32
		nsec := int64(x >> 2)
		if nsec >= 1_000_000_000 {
			return filesystem.Timestamp{}
		}
		return filesystem.NewTimestamp(time.Unix(sec, nsec), time.Nanosecond)
	}
	ft := filesystem.FileTimes{
		Atime: xtime(inodeOffATime, inodeOffATimeExtra),
		Mtime: xtime(inodeOffMTime, inodeOffMTimeExtra),
		Ctime: xtime(inodeOffCTime, inodeOffCTimeExtra),
	}
	if fits(inodeOffCrtime) {
		cr := le.Uint32(raw[inodeOffCrtime:])
		var ex uint32
		if fits(inodeOffCrtimeExtra) {
			ex = le.Uint32(raw[inodeOffCrtimeExtra:])
		}
		if cr != 0 || ex != 0 {
			ft.Btime = xtime(inodeOffCrtime, inodeOffCrtimeExtra)
		}
	}
	return ft
}
