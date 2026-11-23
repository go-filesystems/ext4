package filesystem_ext4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

// Extent tree blocks (ext4_extent_tail) are stamped by the writer and verified
// by the reader. Reference: docs.kernel.org/filesystems/ext4/checksums.html
// and fs/ext4/extents.c (ext4_extent_block_csum).

// hostTool finds an e2fsprogs binary on PATH or under Homebrew's keg-only
// prefix; "" when the host has none.
func hostTool(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, d := range []string{"/opt/homebrew/opt/e2fsprogs/sbin", "/opt/homebrew/sbin", "/opt/homebrew/bin", "/usr/local/sbin", "/usr/sbin", "/sbin"} {
		if p := filepath.Join(d, name); fileExists(p) {
			return p
		}
	}
	return ""
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// extentRootDepth returns the eh_depth of inode num's in-inode extent root and
// the physical block of its first index entry (0 for a depth-0 tree).
func extentRootDepth(t *testing.T, img string, name string) (depth uint16, child uint64, ino uint32) {
	t.Helper()
	f, err := os.Open(img)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dev := &osFileDevice{f: f}
	sb, err := readSuperblockOpts(dev, 0, Options{IgnoreChecksums: true})
	if err != nil {
		t.Fatal(err)
	}
	root, err := readInode(dev, 0, sb, 2)
	if err != nil {
		t.Fatal(err)
	}
	ents, err := readDir(dev, 0, sb, root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if e.Name != name {
			continue
		}
		in, err := readInode(dev, 0, sb, e.Inode)
		if err != nil {
			t.Fatal(err)
		}
		le := binary.LittleEndian
		blk := in.raw[inodeOffBlock : inodeOffBlock+60]
		depth = le.Uint16(blk[6:])
		if depth > 0 {
			child = uint64(le.Uint32(blk[16:])) | uint64(le.Uint16(blk[20:]))<<32
		}
		return depth, child, e.Inode
	}
	t.Fatalf("%s not found in root", name)
	return
}

// flipByte flips one bit of the file at absolute offset off.
func flipByte(t *testing.T, img string, off int64) {
	t.Helper()
	f, err := os.OpenFile(img, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var b [1]byte
	if _, err := f.ReadAt(b[:], off); err != nil {
		t.Fatal(err)
	}
	b[0] ^= 0x01
	if _, err := f.WriteAt(b[:], off); err != nil {
		t.Fatal(err)
	}
}

// scatterWrite writes n small islands 3 blocks apart, so the file has n
// separate extents and its tree needs a block below the inode.
func scatterWrite(t *testing.T, fsys filesystem.Filesystem, path string, from, n int) {
	t.Helper()
	f, err := fsys.(*ext4FS).OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile %s: %v", path, err)
	}
	w := probeWritable(t, f)
	for i := from; i < from+n; i++ {
		if _, err := w.WriteAt(wpattern(100, byte(i)), int64(i)*3*4096); err != nil {
			t.Fatalf("WriteAt island %d: %v", i, err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func checkScatter(t *testing.T, fsys filesystem.Filesystem, path string, n int) {
	t.Helper()
	got, err := fsys.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", path, err)
	}
	for i := 0; i < n; i++ {
		if !bytes.Equal(got[i*3*4096:i*3*4096+100], wpattern(100, byte(i))) {
			t.Fatalf("%s: island %d differs", path, i)
		}
	}
}

// fsckClean runs e2fsck -fn on img and fails on any complaint about checksums
// or extents. Other findings are logged: they are not this change's subject.
func fsckClean(t *testing.T, img string) {
	t.Helper()
	bin := hostTool("e2fsck")
	if bin == "" {
		t.Log("e2fsck not available; interop check skipped")
		return
	}
	out, _ := exec.Command(bin, "-fn", img).CombinedOutput()
	// The image path holds the test's name, which says "Extent".
	low := strings.ToLower(strings.ReplaceAll(string(out), img, "IMG"))
	if strings.Contains(low, "checksum") || strings.Contains(low, "extent") {
		t.Fatalf("e2fsck objects to the image:\n%s", out)
	}
	t.Logf("e2fsck -fn:\n%s", out)
}

// TestExtentBlockCsumWriterStamps builds an image with this package's writer on
// a metadata_csum filesystem: extent growth (sparse islands), truncation, a
// whole-file rewrite, a slow symlink and a hard link. After a reopen with
// verification enforced, every file reads back; e2fsck (when present) has no
// checksum or extent complaint.
func TestExtentBlockCsumWriterStamps(t *testing.T) {
	img := csumImage(t, func([]byte, csumLayout) {})
	fsys, err := Open(img, -1)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile("/scatter.bin", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	scatterWrite(t, fsys, "/scatter.bin", 0, 8) // growth: 8 extents, one leaf block
	scatterWrite(t, fsys, "/scatter.bin", 8, 6) // growth again: leaf rewritten
	if err := fsys.WriteFile("/rewrite.bin", pattern(200000, 3), 0o644); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("d/", 40) + "target" // > 60 bytes: slow symlink
	if err := fsys.(*ext4FS).Symlink(long, "/extcsum.lnk"); err != nil {
		t.Fatal(err)
	}
	if err := fsys.(*ext4FS).Link("/scatter.bin", "/scatter.hard"); err != nil {
		t.Fatal(err)
	}
	// Truncation back to a handful of islands, then a full truncate.
	f, err := fsys.(*ext4FS).OpenFile("/scatter.bin")
	if err != nil {
		t.Fatal(err)
	}
	if err := probeWritable(t, f).Truncate(5 * 3 * 4096); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := fsys.Close(); err != nil {
		t.Fatal(err)
	}

	// Growth + a hole file again so the final image keeps a depth-1 tree.
	fsys, err = Open(img, -1)
	if err != nil {
		t.Fatal(err)
	}
	scatterWrite(t, fsys, "/scatter.bin", 5, 9)
	if err := fsys.Close(); err != nil {
		t.Fatal(err)
	}

	depth, child, _ := extentRootDepth(t, img, "scatter.bin")
	if depth != 1 || child == 0 {
		t.Fatalf("scatter.bin extent depth = %d child = %d; the test needs a tree block", depth, child)
	}

	fsys, err = Open(img, -1) // enforcing verification
	if err != nil {
		t.Fatal(err)
	}
	defer fsys.Close()
	checkScatter(t, fsys, "/scatter.bin", 14)
	if got, err := fsys.ReadFile("/rewrite.bin"); err != nil || !bytes.Equal(got, pattern(200000, 3)) {
		t.Fatalf("rewrite.bin: err %v, equal %v", err, bytes.Equal(got, pattern(200000, 3)))
	}
	if _, err := fsys.ReadFile("/scatter.hard"); err != nil {
		t.Fatalf("hard link: %v", err)
	}
	ents, err := fsys.ListDir("/")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range ents {
		found = found || e.Name() == "extcsum.lnk"
	}
	if !found {
		t.Fatal("slow symlink missing from the root listing")
	}
	fsckClean(t, img)
}

// A flipped bit in an extent block is refused, whether it is in the checksum
// tail or in an entry, and IgnoreChecksums reads the tail-damaged file.
func TestExtentBlockCsumTamperRefused(t *testing.T) {
	img := csumImage(t, func([]byte, csumLayout) {})
	fsys, err := Open(img, -1)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile("/scatter.bin", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	scatterWrite(t, fsys, "/scatter.bin", 0, 9)
	if err := fsys.Close(); err != nil {
		t.Fatal(err)
	}
	depth, child, _ := extentRootDepth(t, img, "scatter.bin")
	if depth != 1 {
		t.Fatalf("depth = %d, want 1", depth)
	}
	const bs = 4096
	tail := int64(child)*bs + 12 + int64(340)*12

	for _, c := range []struct {
		name string
		off  int64
	}{
		{"tail", tail},
		{"entry", int64(child)*bs + 12 + 4}, // ee_len of the first extent
	} {
		t.Run(c.name, func(t *testing.T) {
			dup := filepath.Join(t.TempDir(), "img")
			raw, err := os.ReadFile(img)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dup, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			flipByte(t, dup, c.off)

			fsys, err := Open(dup, -1)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			_, err = fsys.ReadFile("/scatter.bin")
			wantChecksumError(t, err, "extent block")
			// A writer must refuse rather than re-stamp the tampering.
			if f, err := fsys.(*ext4FS).OpenFile("/scatter.bin"); err == nil {
				f.Close()
				t.Fatal("OpenFile accepted a tampered extent block")
			} else {
				wantChecksumError(t, err, "extent block")
			}
			fsys.Close()

			fsys, err = OpenWithOptions(dup, -1, Options{IgnoreChecksums: true})
			if err != nil {
				t.Fatal(err)
			}
			defer fsys.Close()
			if c.name == "tail" {
				checkScatter(t, fsys, "/scatter.bin", 9)
			}
		})
	}
}

// An extent block moved to another inode must not verify: the checksum covers
// the inode number and generation.
func TestExtentBlockCsumCoversInodeAndGeneration(t *testing.T) {
	sb := &superblock{FeatureROCompat: FeatROCompatMetadataCsum, BlockSize: 4096}
	buf := make([]byte, 4096)
	binary.LittleEndian.PutUint16(buf[0:], ExtentMagic)
	binary.LittleEndian.PutUint16(buf[4:], 340)
	stampExtentBlockCsum(sb, 12, 7, buf)
	if err := verifyExtentBlockCsum(sb, 12, 7, 99, buf); err != nil {
		t.Fatalf("stamped block refused: %v", err)
	}
	wantChecksumError(t, verifyExtentBlockCsum(sb, 13, 7, 99, buf), "extent block")
	wantChecksumError(t, verifyExtentBlockCsum(sb, 12, 8, 99, buf), "extent block")
	buf[100] ^= 1
	err := verifyExtentBlockCsum(sb, 12, 7, 99, buf)
	wantChecksumError(t, err, "extent block")
	var ce *ChecksumError
	if errors.As(err, &ce) && (ce.Inode != 12 || ce.Block != 99) {
		t.Fatalf("error does not locate the block: %+v", ce)
	}
	sb.ignoreCsum = true
	if err := verifyExtentBlockCsum(sb, 12, 7, 99, buf); err != nil {
		t.Fatalf("IgnoreChecksums must accept: %v", err)
	}
	// eh_max pointing the tail outside the block is a refusal, not a panic.
	sb.ignoreCsum = false
	binary.LittleEndian.PutUint16(buf[4:], 0xFFFF)
	wantChecksumError(t, verifyExtentBlockCsum(sb, 12, 7, 99, buf), "extent block")
}

// An image made by mke2fs (e2fsprogs) with a fragmented file, so that it has an
// extent block below the inode, is accepted: this is the oracle for the
// algorithm, independent of this package's writer.
func TestExtentBlockCsumAcceptsMke2fsImage(t *testing.T) {
	mke2fs := hostTool("mke2fs")
	if mke2fs == "" {
		t.Skip("mke2fs not available")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatal(err)
	}
	sp, err := os.Create(filepath.Join(src, "scatter.bin"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		if _, err := sp.WriteAt(wpattern(100, byte(i)), int64(i)*3*4096); err != nil {
			t.Fatal(err)
		}
	}
	sp.Close()
	img := filepath.Join(dir, "img")
	if out, err := exec.Command(mke2fs, "-q", "-t", "ext4", "-O", "metadata_csum", "-b", "4096",
		"-d", src, img, "16M").CombinedOutput(); err != nil {
		t.Skipf("mke2fs -d unusable here: %v\n%s", err, out)
	}
	depth, _, _ := extentRootDepth(t, img, "scatter.bin")
	if depth == 0 {
		t.Skip("mke2fs laid the file out inline; no extent block to check")
	}
	fsys, err := Open(img, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer fsys.Close()
	checkScatter(t, fsys, "/scatter.bin", 12)
}

// prepareExtentChild (used by writeFile and directory growth) stamps the block
// it creates for the inode it is given.
func TestPrepareExtentChildStamps(t *testing.T) {
	img := csumImage(t, func([]byte, csumLayout) {})
	f, err := os.OpenFile(img, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dev := &osFileDevice{f: f}
	sb, err := readSuperblockOpts(dev, 0, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ino := &inode{raw: make([]byte, sb.InodeSize), num: 77}
	binary.LittleEndian.PutUint32(ino.raw[inodeOffGeneration:], 0xABCD)
	var exts []extentLeaf
	for i := 0; i < 6; i++ {
		exts = append(exts, extentLeaf{LogBlock: uint32(i * 3), PhysBlock: uint64(2000 + i*5), Count: 1})
	}
	child, cleanup, err := prepareExtentChild(dev, 0, sb, ino, exts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cleanup != nil {
		cleanup()
	}
	buf, err := readRawBlock(dev, 0, sb, child)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyExtentBlockCsum(sb, 77, 0xABCD, child, buf); err != nil {
		t.Fatalf("prepareExtentChild left a bad checksum: %v", err)
	}
	wantChecksumError(t, verifyExtentBlockCsum(sb, 77, 0xABCE, child, buf), "extent block")
}
