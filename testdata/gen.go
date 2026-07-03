//go:build ignore

// Command gen builds the committed ext4 read-path test corpus.
//
// It shells out to the real e2fsprogs oracle (mke2fs / debugfs) to lay down a
// spread of on-disk ext2/ext3/ext4 layouts — different block sizes, extents
// vs. the classic block-map, inline data, fast and slow symlinks, an htree
// directory, a sparse file, and an external xattr — then derives a family of
// deliberately-corrupted images that drive the parser's error branches. Every
// image is packed into testdata/fixtures.tar.gz, which fixtures_test.go embeds
// with go:embed so the corpus travels inside the test binary and runs on every
// arch under QEMU (no mke2fs required at test time).
//
// The generator is deterministic in structure: file contents are produced by a
// fixed pseudo-random stream and every image is built with a pinned UUID and
// hash_seed, so re-running it yields a functionally identical corpus (the only
// bytes that vary are the mke2fs wall-clock timestamps in the superblock, which
// no test inspects). Regenerate with:
//
//	go run ./testdata/gen.go
//
// Requires mke2fs + debugfs (e2fsprogs) on PATH (or in /sbin:/usr/sbin).
package main

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
)

// pinned so the layout (and thus the parser's view) is reproducible.
const (
	fixedUUID     = "5ba3f2c1-0d0d-4a4a-8b8b-1c1c1c1c1c1c"
	fixedHashSeed = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
)

// det fills b with a deterministic byte stream (a tiny LCG) so the corpus is
// reproducible without embedding /dev/urandom output.
func det(b []byte, seed uint32) {
	x := seed*2654435761 + 1
	for i := range b {
		x = x*1664525 + 1013904223
		b[i] = byte(x >> 24)
	}
}

func tool(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, d := range []string{"/sbin", "/usr/sbin", "/usr/local/sbin", "/opt/homebrew/sbin"} {
		c := filepath.Join(d, name)
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	log.Fatalf("gen: %s not found (install e2fsprogs)", name)
	return ""
}

func run(name string, args ...string) {
	cmd := exec.Command(name, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Fatalf("gen: %s %v failed: %v\n%s", name, args, err, out)
	}
}

// buildStage lays down the deterministic source tree that mke2fs -d copies
// into every populated image.
func buildStage(dir string) {
	must := func(err error) {
		if err != nil {
			log.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(dir, "sub", "deep"), 0o755))

	// empty file (zero-length inode).
	must(os.WriteFile(filepath.Join(dir, "empty.txt"), nil, 0o644))

	// ~100 bytes: with inline_data this lands in the in-inode system.data
	// xattr, exercising the inline read path.
	small := make([]byte, 100)
	det(small, 1)
	must(os.WriteFile(filepath.Join(dir, "small_inline.txt"), small, 0o644))

	// multi-block file: one extent / a couple of direct block-map pointers.
	mb := make([]byte, 5000)
	det(mb, 2)
	must(os.WriteFile(filepath.Join(dir, "multiblock.bin"), mb, 0o644))

	// large file: forces several extents, and on ext2/3 (1 KiB blocks) both
	// the single- and double-indirect block-map levels (double-indirect
	// begins at logical block 268 == 274 KiB).
	big := make([]byte, 300000)
	det(big, 3)
	must(os.WriteFile(filepath.Join(dir, "big.bin"), big, 0o644))

	// nested content so ListDir recurses.
	nb := make([]byte, 3000)
	det(nb, 4)
	must(os.WriteFile(filepath.Join(dir, "sub", "deep", "nested.bin"), nb, 0o644))

	// fast symlink: short target stored inline in i_block.
	must(os.Symlink("small_inline.txt", filepath.Join(dir, "fast.lnk")))
	// slow symlink: >60-byte target stored in a data block.
	must(os.Symlink(
		"this/is/a/very/long/symlink/target/path/that/exceeds/sixty/characters/for/slow/symlink/storage",
		filepath.Join(dir, "slow.lnk")))

	// sparse file: a 1 MiB hole followed by a short tail. Reading it must
	// materialise the hole as zeroes.
	sp, err := os.Create(filepath.Join(dir, "sparse.bin"))
	must(err)
	_, err = sp.WriteAt([]byte("END"), 1<<20)
	must(err)
	must(sp.Close())

	// large directory: enough entries to tip the root dir into an htree.
	bd := filepath.Join(dir, "bigdir")
	must(os.MkdirAll(bd, 0o755))
	for i := 0; i < 300; i++ {
		must(os.WriteFile(filepath.Join(bd, fmt.Sprintf("file_%03d", i)), []byte{byte(i)}, 0o644))
	}
}

type spec struct {
	name  string   // output image basename
	extra []string // mke2fs args (fs type / features / block size)
	xattr bool     // inject an external user xattr via debugfs
}

func mkImage(mke2fs, debugfs, stage, out string, s spec) {
	// A fresh sparse file; mke2fs sizes the fs to it.
	f, err := os.Create(out)
	if err != nil {
		log.Fatal(err)
	}
	if err := f.Truncate(16 << 20); err != nil {
		log.Fatal(err)
	}
	f.Close()

	args := append([]string{
		"-q", "-F",
		"-U", fixedUUID,
		"-E", "lazy_itable_init=0,hash_seed=" + fixedHashSeed,
		"-d", stage,
	}, s.extra...)
	args = append(args, out)
	run(mke2fs, args...)

	if s.xattr {
		// External user xattr on a file that also carries inline system.data,
		// forcing the inode xattr walker past multiple entries.
		run(debugfs, "-w", "-R", "ea_set /multiblock.bin user.comment ext4-fixture", out)
	}
}

// patch overwrites len(b) bytes of file at off.
func patch(path string, off int64, b []byte) {
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		log.Fatal(err)
	}
	if _, err := f.WriteAt(b, off); err != nil {
		log.Fatal(err)
	}
	f.Close()
}

func cp(src, dst string) {
	b, err := os.ReadFile(src)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		log.Fatal(err)
	}
}

func main() {
	mke2fs := tool("mke2fs")
	debugfs := tool("debugfs")

	work, err := os.MkdirTemp("", "ext4-fixtures-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(work)

	stage := filepath.Join(work, "stage")
	if err := os.MkdirAll(stage, 0o755); err != nil {
		log.Fatal(err)
	}
	buildStage(stage)

	imgDir := filepath.Join(work, "img")
	if err := os.MkdirAll(imgDir, 0o755); err != nil {
		log.Fatal(err)
	}

	specs := []spec{
		// The flagship: 4 KiB blocks, extents, 64-bit, metadata_csum, inline
		// data, plus an external xattr.
		{name: "ext4_4k.img", xattr: true, extra: []string{
			"-t", "ext4", "-b", "4096",
			"-O", "extents,metadata_csum,64bit,inline_data",
		}},
		// 1 KiB blocks, extents + metadata_csum, no 64-bit / no inline: a
		// different block-size decode path.
		{name: "ext4_1k.img", extra: []string{
			"-t", "ext4", "-b", "1024",
			"-O", "extents,metadata_csum,^64bit,^inline_data",
		}},
		// Classic ext2: block-map (no extents, no journal) — exercises the
		// indirect block-map resolver at 1 KiB blocks.
		{name: "ext2_1k.img", extra: []string{
			"-t", "ext2", "-b", "1024",
		}},
		// ext3: block-map plus an on-disk journal inode.
		{name: "ext3_1k.img", extra: []string{
			"-t", "ext3", "-b", "1024",
		}},
	}

	for _, s := range specs {
		mkImage(mke2fs, debugfs, stage, filepath.Join(imgDir, s.name), s)
	}

	// Corruption corpus, derived from the flagship image. Superblock field
	// offsets are relative to the on-disk superblock, which for any block
	// size >= 1 KiB begins at byte 1024.
	const sbBase = 1024
	base := filepath.Join(imgDir, "ext4_4k.img")

	// bad magic: s_magic @ 0x38.
	c := filepath.Join(imgDir, "corrupt_badmagic.img")
	cp(base, c)
	patch(c, sbBase+0x38, []byte{0x00, 0x00})

	// impossible s_log_block_size (>6) @ 0x18.
	c = filepath.Join(imgDir, "corrupt_badlogbsize.img")
	cp(base, c)
	patch(c, sbBase+0x18, []byte{0x07, 0x00, 0x00, 0x00})

	// s_blocks_per_group == 0 @ 0x20.
	c = filepath.Join(imgDir, "corrupt_zerobpg.img")
	cp(base, c)
	patch(c, sbBase+0x20, []byte{0x00, 0x00, 0x00, 0x00})

	// s_inodes_per_group == 0 @ 0x28.
	c = filepath.Join(imgDir, "corrupt_zeroipg.img")
	cp(base, c)
	patch(c, sbBase+0x28, []byte{0x00, 0x00, 0x00, 0x00})

	// truncated before the superblock even ends: the read itself fails.
	c = filepath.Join(imgDir, "corrupt_truncated.img")
	cp(base, c)
	if err := os.Truncate(c, 512); err != nil {
		log.Fatal(err)
	}

	// garbage: no ext4 anywhere.
	c = filepath.Join(imgDir, "garbage.img")
	g := make([]byte, 8192)
	det(g, 99)
	if err := os.WriteFile(c, g, 0o644); err != nil {
		log.Fatal(err)
	}

	// Pack everything (sorted for a stable archive) into the embedded corpus.
	entries, err := os.ReadDir(imgDir)
	if err != nil {
		log.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	outPath := filepath.Join("testdata", "fixtures.tar.gz")
	tf, err := os.Create(outPath)
	if err != nil {
		log.Fatal(err)
	}
	gz, _ := gzip.NewWriterLevel(tf, gzip.BestCompression)
	tw := tar.NewWriter(gz)
	for _, n := range names {
		data, err := os.ReadFile(filepath.Join(imgDir, n))
		if err != nil {
			log.Fatal(err)
		}
		hdr := &tar.Header{Name: n, Mode: 0o644, Size: int64(len(data))}
		if err := tw.WriteHeader(hdr); err != nil {
			log.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			log.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		log.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		log.Fatal(err)
	}
	if err := tf.Close(); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("gen: wrote %s (%d images)\n", outPath, len(names))
}
