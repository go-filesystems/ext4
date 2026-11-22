package filesystem_ext4

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Checksum verification against a real mke2fs metadata_csum image. Every case
// flips one byte of one metadata structure WITHOUT recomputing its checksum,
// which is exactly what the kernel refuses (EFSBADCRC) and e2fsck reports.

// csumImage copies the 4 KiB metadata_csum fixture, lets mutate edit it, and
// returns its path. The layout argument locates the structures.
type csumLayout struct {
	sb   *superblock
	bgd0 *bgd
}

func csumImage(t *testing.T, mutate func(raw []byte, l csumLayout)) string {
	t.Helper()
	src := filepath.Join(fixtureDir(t), "ext4_4k.img")
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dev := &osFileDevice{f: f}
	sb, err := readSuperblockOpts(dev, 0, Options{IgnoreChecksums: true})
	if err != nil {
		t.Fatal(err)
	}
	d, err := readBGD(dev, 0, sb, 0)
	if err != nil {
		t.Fatal(err)
	}
	mutate(raw, csumLayout{sb: sb, bgd0: d})
	out := filepath.Join(t.TempDir(), "img")
	if err := os.WriteFile(out, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return out
}

func wantChecksumError(t *testing.T, err error, kind string) {
	t.Helper()
	if err == nil {
		t.Fatalf("no error; want a %s checksum error", kind)
	}
	var ce *ChecksumError
	if !errors.As(err, &ce) {
		t.Fatalf("error %v is not a *ChecksumError", err)
	}
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("errors.Is(%v, ErrChecksum) = false", err)
	}
	if ce.Kind != kind {
		t.Fatalf("kind = %q, want %q (%v)", ce.Kind, kind, err)
	}
}

func TestChecksumPristineImageOpens(t *testing.T) {
	img := csumImage(t, func([]byte, csumLayout) {})
	fsys, err := Open(img, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer fsys.Close()
	if _, err := fsys.ListDir("/"); err != nil {
		t.Fatal(err)
	}
}

func TestChecksumSuperblock(t *testing.T) {
	img := csumImage(t, func(raw []byte, _ csumLayout) { raw[1024+0x30] ^= 0xFF }) // s_wtime
	_, err := Open(img, -1)
	wantChecksumError(t, err, "superblock")

	fsys, err := OpenWithOptions(img, -1, Options{IgnoreChecksums: true})
	if err != nil {
		t.Fatalf("IgnoreChecksums must open it: %v", err)
	}
	fsys.Close()
}

func TestChecksumGroupDescriptor(t *testing.T) {
	img := csumImage(t, func(raw []byte, l csumLayout) {
		raw[int64(l.sb.bgdTableBlock())*int64(l.sb.BlockSize)+12] ^= 0xFF // bg_free_blocks_count_lo
	})
	fsys, err := Open(img, -1)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fsys.ListDir("/")
	wantChecksumError(t, err, "group descriptor")
	fsys.Close()

	fsys, err = OpenWithOptions(img, -1, Options{IgnoreChecksums: true})
	if err != nil {
		t.Fatal(err)
	}
	defer fsys.Close()
	if _, err := fsys.ListDir("/"); err != nil {
		t.Fatalf("IgnoreChecksums must read it: %v", err)
	}
}

func TestChecksumInode(t *testing.T) {
	img := csumImage(t, func(raw []byte, l csumLayout) {
		root := int64(l.bgd0.InodeTableBlock)*int64(l.sb.BlockSize) + int64(l.sb.InodeSize) // inode 2
		raw[root+16] ^= 0xFF                                                                // i_ctime
	})
	fsys, err := Open(img, -1)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fsys.ListDir("/")
	wantChecksumError(t, err, "inode")
	fsys.Close()

	fsys, err = OpenWithOptions(img, -1, Options{IgnoreChecksums: true})
	if err != nil {
		t.Fatal(err)
	}
	defer fsys.Close()
	if _, err := fsys.ListDir("/"); err != nil {
		t.Fatalf("IgnoreChecksums must read it: %v", err)
	}
}

// The tail's own checksum bytes altered, entries untouched.
func TestChecksumDirectoryTail(t *testing.T) {
	var blk int64
	img := csumImage(t, func(raw []byte, l csumLayout) {
		bs := int64(l.sb.BlockSize)
		// Root directory data block: the one whose last 12 bytes are a tail
		// and whose first dirent is inode 2 ("."), i.e. mke2fs's root block.
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
	fsys, err := Open(img, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer fsys.Close()
	_, err = fsys.ListDir("/")
	wantChecksumError(t, err, "directory block")
}

// A tampered bitmap must be refused by the allocator, not re-stamped by it.
func TestChecksumBlockBitmapRefusedOnWrite(t *testing.T) {
	img := csumImage(t, func(raw []byte, l csumLayout) {
		raw[int64(l.bgd0.BlockBitmapBlock)*int64(l.sb.BlockSize)+3] ^= 0x55
	})
	fsys, err := Open(img, -1)
	if err != nil {
		t.Fatal(err)
	}
	err = fsys.WriteFile("/new.bin", []byte("data"), 0o644)
	fsys.Close()
	wantChecksumError(t, err, "block bitmap")
}

func TestChecksumInodeBitmapRefusedOnWrite(t *testing.T) {
	img := csumImage(t, func(raw []byte, l csumLayout) {
		raw[int64(l.bgd0.InodeBitmapBlock)*int64(l.sb.BlockSize)+1] ^= 0x55
	})
	fsys, err := Open(img, -1)
	if err != nil {
		t.Fatal(err)
	}
	err = fsys.WriteFile("/new.bin", []byte("data"), 0o644)
	fsys.Close()
	wantChecksumError(t, err, "inode bitmap")
}

// A write to a pristine image must leave every checksum valid: re-open and
// re-read everything with enforcement on.
func TestChecksumWritesStayVerifiable(t *testing.T) {
	img := csumImage(t, func([]byte, csumLayout) {})
	fsys, err := Open(img, -1)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile("/new.bin", []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fsys.MkDir("/csumdir", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Close(); err != nil {
		t.Fatal(err)
	}
	fsys, err = Open(img, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer fsys.Close()
	got, err := fsys.ReadFile("/new.bin")
	if err != nil || string(got) != "data" {
		t.Fatalf("ReadFile = %q, %v", got, err)
	}
	if _, err := fsys.ListDir("/csumdir"); err != nil {
		t.Fatal(err)
	}
}

func TestCRC16Vector(t *testing.T) {
	// CRC-16/ARC check value (the kernel's lib/crc16 with init 0).
	if got := crc16(0, []byte("123456789")); got != 0xBB3D {
		t.Fatalf("crc16 = %#x, want 0xBB3D", got)
	}
}

func TestGdtCsumLegacyDescriptor(t *testing.T) {
	sb := &superblock{FeatureROCompat: FeatROCompatGdtCsum, DescSize: 32}
	sb.UUID = [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	raw := make([]byte, 32)
	copy(raw, "descriptor body bytes")
	stampBGDCsum(sb, 7, raw)
	if err := verifyBGDCsum(sb, 7, raw); err != nil {
		t.Fatal(err)
	}
	raw[5] ^= 1
	wantChecksumError(t, verifyBGDCsum(sb, 7, raw), "group descriptor")
	if err := verifyBGDCsum(&superblock{FeatureROCompat: FeatROCompatGdtCsum, ignoreCsum: true}, 7, raw); err != nil {
		t.Fatalf("ignored: %v", err)
	}
}

// Forged structures must never panic the verifiers.
func TestChecksumVerifiersNeverPanic(t *testing.T) {
	sb := &superblock{FeatureROCompat: FeatROCompatMetadataCsum, DescSize: 64, BlockSize: 4096, BlocksPerGroup: 32768, InodesPerGroup: 8192}
	for _, n := range []int{0, 1, 11, 12, 39, 40, 128, 4096} {
		buf := make([]byte, n)
		for i := range buf {
			buf[i] = 0xFF
		}
		_ = verifyDirBlockCsum(sb, 2, 0, 1, buf)
		_ = verifyInodeCsum(sb, 2, buf)
		_ = verifyBGDCsum(sb, 0, buf)
	}
}
