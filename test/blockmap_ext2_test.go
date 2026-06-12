package filesystem_ext4_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	ext4 "github.com/go-filesystems/ext4"
)

// findTool resolves a (possibly sbin-only) tool, mirroring makeTestImage's
// lookup so the test runs on Linux CI where mkfs tools live in /usr/sbin.
func findTool(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, d := range []string{"/usr/local/sbin", "/usr/sbin", "/sbin", "/opt/homebrew/sbin"} {
		c := filepath.Join(d, name)
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// TestExt2BlockMap_Read formats a real ext2 image (block maps, no extents)
// populated from a directory tree, then verifies the driver reads files that
// span direct, single- and double-indirect blocks, plus a sub-directory
// listing — all via the classic block-map path.
func TestExt2BlockMap_Read(t *testing.T) {
	mke2fs := findTool("mke2fs")
	if mke2fs == "" {
		t.Skip("mke2fs not available — skipping ext2 block-map test")
	}

	// Build a source tree.
	src := t.TempDir()
	small := []byte("hello ext2 block map\n")
	// 1 MiB deterministic pattern: with -b 1024 this spans direct (12 KiB),
	// single-indirect (256 KiB) and into double-indirect.
	big := make([]byte, 1<<20)
	for i := range big {
		big[i] = byte(i % 251)
	}
	nested := []byte("nested file\n")
	if err := os.WriteFile(filepath.Join(src, "small.txt"), small, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "nested.txt"), nested, 0o644); err != nil {
		t.Fatal(err)
	}

	// Format a bare ext2 image populated from src.
	img := filepath.Join(t.TempDir(), "ext2.img")
	if f, err := os.Create(img); err != nil {
		t.Fatal(err)
	} else {
		f.Truncate(16 * 1024 * 1024)
		f.Close()
	}
	cmd := exec.Command(mke2fs, "-t", "ext2", "-b", "1024", "-F", "-L", "test", "-d", src, img)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("mke2fs failed: %v\n%s", err, out)
	}

	fs, err := ext4.Open(img, -1)
	if err != nil {
		t.Fatalf("Open ext2 image: %v", err)
	}
	defer fs.Close()

	for _, tc := range []struct {
		path string
		want []byte
	}{
		{"/small.txt", small},
		{"/big.bin", big},
		{"/sub/nested.txt", nested},
	} {
		got, err := fs.ReadFile(tc.path)
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", tc.path, err)
		}
		if !bytes.Equal(got, tc.want) {
			t.Fatalf("ReadFile(%s): %d bytes, want %d (first-diff check)", tc.path, len(got), len(tc.want))
		}
	}

	// Directory listing also goes through the block-map read path.
	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir(/): %v", err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.Name()] = true
	}
	for _, name := range []string{"small.txt", "big.bin", "sub"} {
		if !seen[name] {
			t.Fatalf("ListDir(/) missing %q (got %v)", name, keys(seen))
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
