package filesystem_ext4

import (
	"os"
	"testing"
)

// The directory-mutating helpers must refuse a directory block whose checksum
// does not match, instead of editing it and stamping a fresh, valid checksum
// over the tampering. They are called directly: the public entry points
// resolve the path first and would be refused earlier, which would hide a
// missing guard here.
func tamperedRootDir(t *testing.T) (*osFileDevice, *superblock, *inode) {
	t.Helper()
	img := csumImage(t, func(raw []byte, l csumLayout) {
		bs := int64(l.sb.BlockSize)
		var blk int64
		for b := int64(1); b < int64(len(raw))/bs && blk == 0; b++ {
			blkBuf := raw[b*bs : (b+1)*bs]
			if hasDirTail(blkBuf) && blkBuf[0] == 2 && blkBuf[4] == 12 && blkBuf[8] == '.' {
				blk = b
			}
		}
		if blk == 0 {
			t.Fatal("root block not found")
		}
		raw[(blk+1)*bs-1] ^= 0xFF
	})
	f, err := os.OpenFile(img, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	dev := &osFileDevice{f: f}
	sb, err := readSuperblockOpts(dev, 0, Options{})
	if err != nil {
		t.Fatal(err)
	}
	root, err := readInode(dev, 0, sb, 2)
	if err != nil {
		t.Fatal(err)
	}
	return dev, sb, root
}

func TestChecksumDirWritersRefuseTamperedBlock(t *testing.T) {
	dev, sb, root := tamperedRootDir(t)
	wantChecksumError(t, zeroDirEntry(dev, 0, sb, root, "lost+found"), "directory block")
	wantChecksumError(t, addDirEntry(dev, 0, sb, root, 12, "zzz", FtRegFile), "directory block")
	wantChecksumError(t, updateDotDot(dev, 0, sb, root, 2), "directory block")
}
