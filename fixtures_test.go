package filesystem_ext4

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	_ "embed"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The committed corpus is a set of real ext2/ext3/ext4 images produced by the
// e2fsprogs oracle (see testdata/gen.go). Embedding it means the pure-Go
// decode/parse path runs on every arch under QEMU — including big-endian s390x,
// where the little-endian on-disk superblock / inode / extent / directory /
// block-map decoders must still yield correct values. No mke2fs is needed at
// test time, so these tests do not skip on the emulated CI runners.
//
//go:embed testdata/fixtures.tar.gz
var fixturesTarGz []byte

// det reproduces the deterministic byte stream that testdata/gen.go wrote into
// each staged file, so the read path can be verified byte-for-byte.
func det(b []byte, seed uint32) {
	x := seed*2654435761 + 1
	for i := range b {
		x = x*1664525 + 1013904223
		b[i] = byte(x >> 24)
	}
}

// fixtureDir extracts the embedded corpus into a fresh temp directory and
// returns the directory path. Each image is a plain file named e.g.
// "ext4_4k.img".
func fixtureDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gz, err := gzip.NewReader(bytes.NewReader(fixturesTarGz))
	if err != nil {
		t.Fatalf("open corpus gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read corpus tar: %v", err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read corpus member %s: %v", hdr.Name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, hdr.Name), data, 0o644); err != nil {
			t.Fatalf("materialise %s: %v", hdr.Name, err)
		}
	}
	return dir
}

// detFile returns the expected contents of a det()-seeded staged file.
func detFile(seed uint32, n int) []byte {
	b := make([]byte, n)
	det(b, seed)
	return b
}

// goodImages are the four healthy fixtures; all four were populated from the
// identical staging tree, so the read assertions are shared.
var goodImages = []string{"ext4_4k.img", "ext4_1k.img", "ext2_1k.img", "ext3_1k.img"}

// TestFixtureRead walks every healthy image and verifies the full read path:
// directory listing (incl. an htree directory), regular-file contents at
// several sizes, extents vs. the ext2/3 indirect block map, inline data, a
// sparse hole, and both fast and slow symlinks.
func TestFixtureRead(t *testing.T) {
	dir := fixtureDir(t)
	for _, img := range goodImages {
		img := img
		t.Run(img, func(t *testing.T) {
			fs, err := Open(filepath.Join(dir, img), -1)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer fs.Close()

			// Root listing must contain the staged names.
			ents, err := fs.ListDir("/")
			if err != nil {
				t.Fatalf("ListDir /: %v", err)
			}
			got := map[string]bool{}
			for _, e := range ents {
				got[e.Name()] = true
			}
			for _, want := range []string{
				"empty.txt", "small_inline.txt", "multiblock.bin", "big.bin",
				"sub", "fast.lnk", "slow.lnk", "sparse.bin", "bigdir", "lost+found",
			} {
				if !got[want] {
					t.Errorf("root missing %q (have %v)", want, keys(got))
				}
			}

			// Regular files at several sizes: empty, inline-sized, multi-block,
			// and large (multi-extent / double-indirect block map).
			checkFile(t, fs, "/empty.txt", nil)
			checkFile(t, fs, "/small_inline.txt", detFile(1, 100))
			checkFile(t, fs, "/multiblock.bin", detFile(2, 5000))
			checkFile(t, fs, "/big.bin", detFile(3, 300000))
			checkFile(t, fs, "/sub/deep/nested.bin", detFile(4, 3000))

			// Sparse file: a 1 MiB hole materialised as zeros, then "END".
			sp, err := fs.ReadFile("/sparse.bin")
			if err != nil {
				t.Fatalf("ReadFile sparse: %v", err)
			}
			if len(sp) != (1<<20)+3 {
				t.Fatalf("sparse size = %d, want %d", len(sp), (1<<20)+3)
			}
			for i := 0; i < 1<<20; i++ {
				if sp[i] != 0 {
					t.Fatalf("sparse hole byte %d = %d, want 0", i, sp[i])
				}
			}
			if string(sp[1<<20:]) != "END" {
				t.Fatalf("sparse tail = %q, want END", sp[1<<20:])
			}

			// Fast (inline) and slow (out-of-line) symlinks.
			if tgt, err := fs.ReadLink("/fast.lnk"); err != nil || tgt != "small_inline.txt" {
				t.Fatalf("ReadLink fast.lnk = %q, %v", tgt, err)
			}
			const longTarget = "this/is/a/very/long/symlink/target/path/that/exceeds/sixty/characters/for/slow/symlink/storage"
			if tgt, err := fs.ReadLink("/slow.lnk"); err != nil || tgt != longTarget {
				t.Fatalf("ReadLink slow.lnk = %q, %v", tgt, err)
			}

			// htree directory: 300 entries.
			bd, err := fs.ListDir("/bigdir")
			if err != nil {
				t.Fatalf("ListDir /bigdir: %v", err)
			}
			if len(bd) != 300 {
				t.Fatalf("bigdir entries = %d, want 300", len(bd))
			}

			// Stat a file and a directory.
			st, err := fs.Stat("/big.bin")
			if err != nil || st.Size() != 300000 {
				t.Fatalf("Stat big.bin size = %d, err %v", stSize(st), err)
			}
			if _, err := fs.Stat("/sub"); err != nil {
				t.Fatalf("Stat /sub: %v", err)
			}

			// A missing path must be a clean error, not a panic.
			if _, err := fs.ReadFile("/does/not/exist"); err == nil {
				t.Fatal("ReadFile missing path: want error")
			}
		})
	}
}

func checkFile(t *testing.T, fs interface {
	ReadFile(string) ([]byte, error)
}, path string, want []byte) {
	t.Helper()
	got, err := fs.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("ReadFile %s: %d bytes, mismatch (want %d)", path, len(got), len(want))
	}
}

// corruptCase pairs a fixture with a substring its Open error must contain.
var corruptCases = []struct {
	img     string
	errFrag string
}{
	{"corrupt_badmagic.img", "magic"},
	{"corrupt_badlogbsize.img", "s_log_block_size"},
	{"corrupt_zerobpg.img", "s_blocks_per_group"},
	{"corrupt_zeroipg.img", "s_inodes_per_group"},
	{"corrupt_truncated.img", "superblock"},
	{"garbage.img", "magic"},
}

// TestFixtureCorruption drives the superblock parser's error branches with
// images whose on-disk bytes were deliberately damaged by the generator.
func TestFixtureCorruption(t *testing.T) {
	dir := fixtureDir(t)
	for _, tc := range corruptCases {
		tc := tc
		t.Run(tc.img, func(t *testing.T) {
			fs, err := Open(filepath.Join(dir, tc.img), -1)
			if err == nil {
				fs.Close()
				t.Fatalf("Open %s: want error, got nil", tc.img)
			}
			if !strings.Contains(err.Error(), "ext4:") {
				t.Fatalf("Open %s: error %q lacks ext4: prefix", tc.img, err)
			}
			if !strings.Contains(err.Error(), tc.errFrag) {
				t.Fatalf("Open %s: error %q missing %q", tc.img, err, tc.errFrag)
			}
		})
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// stSize tolerates a nil Stat when reporting an error.
func stSize(s interface{ Size() uint64 }) uint64 {
	if s == nil {
		return 0
	}
	return s.Size()
}
