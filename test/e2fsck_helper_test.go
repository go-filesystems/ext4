package filesystem_ext4_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	ext4 "github.com/go-filesystems/ext4"
)

// lookupE2fsck returns the absolute path to an e2fsck binary, or "" when
// none is available on the host. It searches PATH first, then a small list
// of common locations on macOS/Linux where e2fsprogs is typically installed
// under a non-default sbin prefix.
func lookupE2fsck() string {
	if p, err := exec.LookPath("e2fsck"); err == nil {
		return p
	}
	for _, c := range []string{
		"/opt/homebrew/sbin/e2fsck",
		"/usr/local/sbin/e2fsck",
		"/usr/sbin/e2fsck",
		"/sbin/e2fsck",
	} {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// runE2fsck shells out to `e2fsck -f -n <img>` and asserts that the image is
// clean: exit code 0, and stdout/stderr free of `ERROR:` lines. If e2fsck is
// not installed on the host, the test is skipped via t.Skip so it can run on
// CI environments without e2fsprogs without producing a failure.
//
// The "-f" flag forces a check even if the filesystem is marked clean, and
// "-n" puts e2fsck into non-modify mode so the image on disk is never
// touched. This makes the helper safe to apply to a temp image opened by an
// open *Ext4FS as well, provided the caller has flushed/closed it first.
func runE2fsck(t *testing.T, img string) {
	t.Helper()
	bin := lookupE2fsck()
	if bin == "" {
		t.Skip("e2fsck not available on PATH; skipping cross-validation")
		return
	}
	cmd := exec.Command(bin, "-f", "-n", img)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("e2fsck %s reported errors (%v):\n%s", img, err, string(out))
	}
	// Some old e2fsck builds exit 0 but still log ERROR: lines for soft
	// problems. Treat any such line as a failure to match the cross-compat
	// audit expectation. Also catch the diagnostics e2fsck prints for
	// inconsistencies it *would* repair (e.g. "Free blocks count wrong",
	// "Free inodes count wrong") and the interactive "Fix? no" / "Fix<y>?"
	// prompts that accompany them: under `-n` these accompany a non-zero
	// exit, but we assert on them explicitly so a future change that
	// swallows the exit code can never let such an image be reported clean.
	for _, marker := range [][]byte{
		[]byte("ERROR:"),
		[]byte("count wrong"),
		[]byte("Fix? no"),
		[]byte("Fix<y>?"),
	} {
		if bytes.Contains(out, marker) {
			t.Fatalf("e2fsck reported %q on %s:\n%s", string(marker), img, string(out))
		}
	}
}

// flushAndE2fsck closes the open *Ext4FS to flush pending writes to the
// backing file, then runs e2fsck against the image path. It is the common
// "writer round-trip → e2fsck" pattern used by all writer cross-validation
// tests below.
func flushAndE2fsck(t *testing.T, fs *ext4.Ext4FS, img string) {
	t.Helper()
	if err := fs.Close(); err != nil {
		t.Fatalf("Close before e2fsck: %v", err)
	}
	runE2fsck(t, img)
}

// formatImg formats a fresh ext4 image at `img` with default config and the
// requested size. Used by the e2fsck cross-validation tests; intentionally
// kept small to avoid being mistaken for a generic helper.
func formatImg(t *testing.T, img string, size int64) *ext4.Ext4FS {
	t.Helper()
	fsi, err := ext4.Format(img, size, ext4.FormatConfig{})
	if err != nil {
		t.Fatalf("Format(%q, %d): %v", img, size, err)
	}
	fs, ok := fsi.(*ext4.Ext4FS)
	if !ok {
		fsi.Close()
		t.Fatalf("Format returned non-ext4 implementation")
	}
	return fs
}

// ── e2fsck cross-validation tests ───────────────────────────────────────────
//
// These tests format an image with our writer, optionally mutate it, then
// shell out to upstream e2fsck to confirm the on-disk layout is structurally
// valid. They are skipped when e2fsck is not installed.

// TestE2fsckOnEmptyFormat: Format an empty image, close it, validate with
// e2fsck. This is the most minimal writer round-trip and exercises the
// Format path end-to-end.
func TestE2fsckOnEmptyFormat(t *testing.T) {
	img := filepath.Join(t.TempDir(), "empty.img")
	fs := formatImg(t, img, 20*1024*1024)
	flushAndE2fsck(t, fs, img)
}

// TestE2fsckAfterWriteFile: Format → WriteFile → close → e2fsck.
func TestE2fsckAfterWriteFile(t *testing.T) {
	img := filepath.Join(t.TempDir(), "write.img")
	fs := formatImg(t, img, 20*1024*1024)
	if err := fs.WriteFile("/hello.txt", []byte("hello from writer\n"), 0o644); err != nil {
		fs.Close()
		t.Fatalf("WriteFile: %v", err)
	}
	flushAndE2fsck(t, fs, img)
}

// TestE2fsckAfterMultipleFiles: a few small files, then close → e2fsck.
func TestE2fsckAfterMultipleFiles(t *testing.T) {
	img := filepath.Join(t.TempDir(), "multi.img")
	fs := formatImg(t, img, 20*1024*1024)
	files := map[string]string{
		"/a.txt": "alpha",
		"/b.txt": strings.Repeat("beta ", 200),
		"/c.txt": "gamma",
	}
	for name, data := range files {
		if err := fs.WriteFile(name, []byte(data), 0o644); err != nil {
			fs.Close()
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}
	flushAndE2fsck(t, fs, img)
}

// TestE2fsckAfterMkDir: Format → MkDir → WriteFile in subdir → close → e2fsck.
func TestE2fsckAfterMkDir(t *testing.T) {
	img := filepath.Join(t.TempDir(), "mkdir.img")
	fs := formatImg(t, img, 20*1024*1024)
	if err := fs.MkDir("/sub", 0o755); err != nil {
		fs.Close()
		t.Fatalf("MkDir: %v", err)
	}
	if err := fs.WriteFile("/sub/nested.txt", []byte("nested"), 0o644); err != nil {
		fs.Close()
		t.Fatalf("WriteFile in subdir: %v", err)
	}
	flushAndE2fsck(t, fs, img)
}

// TestE2fsckAfterSlowSymlink: Format → create a >60-byte (block-backed, "slow")
// symlink → close → e2fsck. Regression guard for the off-by-one free-block
// count the slow-symlink path used to leave in the superblock; previously
// e2fsck reported "Free blocks count wrong". Also creates a short (fast,
// inline) symlink and a normal file in the same image to confirm those paths
// stay clean alongside it.
func TestE2fsckAfterSlowSymlink(t *testing.T) {
	img := filepath.Join(t.TempDir(), "symlink.img")
	fs := formatImg(t, img, 20*1024*1024)
	slowTarget := "/" + strings.Repeat("abcdefghij/", 12) + "leaf" // 137 bytes > 60
	if len(slowTarget) <= 60 {
		fs.Close()
		t.Fatalf("slow-symlink target too short (%d) to exercise the block path", len(slowTarget))
	}
	if err := fs.Symlink(slowTarget, "/slowlink"); err != nil {
		fs.Close()
		t.Fatalf("Symlink (slow): %v", err)
	}
	if err := fs.Symlink("short", "/fastlink"); err != nil {
		fs.Close()
		t.Fatalf("Symlink (fast): %v", err)
	}
	if err := fs.WriteFile("/regular.txt", []byte("plain file"), 0o644); err != nil {
		fs.Close()
		t.Fatalf("WriteFile: %v", err)
	}
	flushAndE2fsck(t, fs, img)
}

// TestE2fsckAfterMultiGroup: Format a >128 MiB image (forces nGroups > 1),
// write a file, close, e2fsck. Catches BGD/bitmap/csum issues in any of the
// secondary block groups.
func TestE2fsckAfterMultiGroup(t *testing.T) {
	img := filepath.Join(t.TempDir(), "multigroup.img")
	const size = 256 * 1024 * 1024
	fs := formatImg(t, img, size)
	if err := fs.WriteFile("/multi.txt", []byte("multi-group test"), 0o644); err != nil {
		fs.Close()
		t.Fatalf("WriteFile in multi-group image: %v", err)
	}
	flushAndE2fsck(t, fs, img)
}

// TestE2fsckAfterReopenAndWrite: Format → close → re-Open → WriteFile →
// close → e2fsck. Exercises the Open-then-write path, which historically
// has had different journal/super flush behavior than the Format-then-write
// path.
func TestE2fsckAfterReopenAndWrite(t *testing.T) {
	img := filepath.Join(t.TempDir(), "reopen.img")
	fs := formatImg(t, img, 20*1024*1024)
	if err := fs.Close(); err != nil {
		t.Fatalf("Close after Format: %v", err)
	}
	fsi, err := ext4.Open(img, -1)
	if err != nil {
		t.Fatalf("Open after Format: %v", err)
	}
	fs2, ok := fsi.(*ext4.Ext4FS)
	if !ok {
		fsi.Close()
		t.Fatalf("Open returned non-ext4 implementation")
	}
	if err := fs2.WriteFile("/post-open.txt", []byte("written after re-open"), 0o600); err != nil {
		fs2.Close()
		t.Fatalf("WriteFile after re-open: %v", err)
	}
	flushAndE2fsck(t, fs2, img)
}
