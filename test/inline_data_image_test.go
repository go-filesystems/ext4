package filesystem_ext4_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	ext4 "github.com/go-filesystems/ext4"
)

// findMke2fs locates an mke2fs binary, mirroring makeTestImage's discovery
// logic. It returns "" (and skips the test) if none is found.
func findMke2fs(t *testing.T) string {
	t.Helper()
	if p, err := exec.LookPath("mke2fs"); err == nil {
		return p
	}
	for _, c := range []string{
		"/usr/sbin/mke2fs",
		"/sbin/mke2fs",
		"/usr/local/sbin/mke2fs",
		"/opt/homebrew/sbin/mke2fs",
	} {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	t.Skip("mke2fs not available — skipping inline_data image test")
	return ""
}

// TestInlineData_RealImage formats a small ext4 image with the inline_data
// feature enabled, populated from a source directory via mke2fs -d, then reads
// a small inline file and lists an inline directory through the public API.
func TestInlineData_RealImage(t *testing.T) {
	mke2fs := findMke2fs(t)

	// Build a source tree with small files (which mke2fs stores inline when
	// inline_data is enabled and the content fits in the inode).
	src := t.TempDir()
	smallContent := []byte("inline content stored in the inode\n")
	if err := os.WriteFile(filepath.Join(src, "small.txt"), smallContent, 0o644); err != nil {
		t.Fatalf("write small.txt: %v", err)
	}
	sub := filepath.Join(src, "dir")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("mkdir dir: %v", err)
	}
	subContent := []byte("nested\n")
	if err := os.WriteFile(filepath.Join(sub, "nested.txt"), subContent, 0o644); err != nil {
		t.Fatalf("write nested.txt: %v", err)
	}

	img := filepath.Join(t.TempDir(), "inline.img")
	f, err := os.Create(img)
	if err != nil {
		t.Fatalf("create image: %v", err)
	}
	if err := f.Truncate(8 * 1024 * 1024); err != nil {
		f.Close()
		t.Fatalf("truncate: %v", err)
	}
	f.Close()

	// 1 KiB blocks + inline_data so small files/dirs are stored in the inode.
	cmd := exec.Command(mke2fs,
		"-t", "ext4",
		"-F",
		"-b", "1024",
		"-O", "inline_data,extents,metadata_csum",
		"-E", "lazy_itable_init=0,lazy_journal_init=0",
		"-d", src,
		"-L", "inlinetest",
		img,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("mke2fs -O inline_data failed (feature/tooling unavailable): %v\n%s", err, out)
	}

	fs, err := ext4.Open(img, -1)
	if err != nil {
		t.Fatalf("open image: %v", err)
	}
	defer fs.Close()

	// Read the small inline file.
	got, err := fs.ReadFile("/small.txt")
	if err != nil {
		t.Fatalf("ReadFile /small.txt: %v", err)
	}
	if !bytes.Equal(got, smallContent) {
		t.Fatalf("/small.txt = %q, want %q", got, smallContent)
	}

	// List the root directory (which is itself typically inline here) and
	// confirm both entries are present.
	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir /: %v", err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name()] = true
	}
	for _, want := range []string{"small.txt", "dir"} {
		if !names[want] {
			t.Fatalf("root listing missing %q; got %v", want, names)
		}
	}

	// Read the nested file inside the subdirectory.
	gotNested, err := fs.ReadFile("/dir/nested.txt")
	if err != nil {
		t.Fatalf("ReadFile /dir/nested.txt: %v", err)
	}
	if !bytes.Equal(gotNested, subContent) {
		t.Fatalf("/dir/nested.txt = %q, want %q", gotNested, subContent)
	}
}
