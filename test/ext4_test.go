package filesystem_ext4_test

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	ext4 "github.com/go-filesystems/ext4"
)

// rockyRawPath returns the path to a raw disk image converted from the Rocky 10
// qcow2 cached by mock. The raw file is stored alongside the qcow2 and reused
// between runs. The test is skipped when:
//   - the Rocky 10 qcow2 is not found in the mock cache, or
//   - qemu-img is not available on the host.
func rockyRawPath(t *testing.T) string {
	t.Helper()

	// Locate the Rocky 10 qcow2 in the mock image cache.
	home := os.Getenv("HOME")
	candidates := []string{
		filepath.Join(home, ".mock", "cache",
			"https____dl.rockylinux.org_pub_rocky_10_images_aarch64_Rocky-10-GenericCloud-Base.latest.aarch64.qcow2",
			"Rocky-10-GenericCloud-Base.latest.aarch64.qcow2"),
		filepath.Join(home, ".mock", "cache",
			"https____dl.rockylinux.org_pub_rocky_10_images_aarch64_Rocky-10-GenericCloud.latest.aarch64.qcow2",
			"Rocky-10-GenericCloud.latest.aarch64.qcow2"),
		filepath.Join(home, ".mock", "cache",
			"https____dl.rockylinux.org_pub_rocky_10_images_aarch64_Rocky-10-GenericCloud.qcow2",
			"Rocky-10-GenericCloud.qcow2"),
	}
	var qcow2 string
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			qcow2 = p
			break
		}
	}
	if qcow2 == "" {
		// Try to download the candidate images into the mock cache.
		for _, p := range candidates {
			enc := filepath.Base(filepath.Dir(p))
			fname := filepath.Base(p)
			if got, err := ensureImageInCache(t, enc, fname); err == nil {
				qcow2 = got
				break
			} else {
				t.Logf("ensureImageInCache failed for %s/%s: %v", enc, fname, err)
			}
		}
		if qcow2 == "" {
			t.Skip("no Rocky 10 qcow2 found in mock cache — tried to download but failed; run 'mock pull' manually")
			return ""
		}
	}

	// Locate qemu-img for qcow2 → raw conversion.
	qemuImg, err := exec.LookPath("qemu-img")
	if err != nil {
		for _, c := range []string{
			"/opt/homebrew/bin/qemu-img",
			"/usr/local/bin/qemu-img",
			"/usr/bin/qemu-img",
		} {
			if _, err2 := os.Stat(c); err2 == nil {
				qemuImg = c
				err = nil
				break
			}
		}
	}
	if err != nil {
		t.Skip("qemu-img not available — install qemu to run Rocky image tests")
		return ""
	}

	// Raw image is cached alongside the qcow2; regenerated if stale.
	raw := strings.TrimSuffix(qcow2, ".qcow2") + "-ext4test.raw"
	qi, _ := os.Stat(qcow2)
	ri, rerr := os.Stat(raw)
	if rerr != nil || (qi != nil && ri.ModTime().Before(qi.ModTime())) {
		t.Logf("converting %s to raw (first run or stale)", filepath.Base(qcow2))
		cmd := exec.Command(qemuImg, "convert", "-f", "qcow2", "-O", "raw", qcow2, raw)
		if out, cerr := cmd.CombinedOutput(); cerr != nil {
			t.Logf("qemu-img convert failed: %v\n%s", cerr, out)
			t.Skip("qemu-img convert failed — skipping image-dependent test")
			return ""
		}
	}
	return raw
}

// rockyExt4PartIdx returns the partition index of the first ext4 partition in
// rawPath, trying auto-detect (-1) then explicit indices 0–7.
func rockyExt4PartIdx(t *testing.T, rawPath string) int {
	t.Helper()
	// Operate on a temporary copy to avoid touching the cached raw image.
	tmp := copyImage(t, rawPath)
	if fs, err := ext4.Open(tmp, -1); err == nil {
		fs.Close()
		return -1
	}
	for i := 0; i < 8; i++ {
		if fs, err := ext4.Open(tmp, i); err == nil {
			fs.Close()
			return i
		}
	}
	t.Skip("no ext4 partition found in Rocky 10 image")
	return -1
}

// ---------------------------------------------------------------------------
// makeTestImage creates a temporary ext4 filesystem image using mke2fs.
// The image is a bare ext4 FS (no partition table), so Open with partIndex=-1
// will detect it as a bare image at offset 0.
// The returned path is automatically removed when the test ends.
// The test is skipped if mke2fs is not available.
// ---------------------------------------------------------------------------

func makeTestImage(t *testing.T, sizeMB int) string {
	t.Helper()
	mke2fs, err := exec.LookPath("mke2fs")
	if err != nil {
		// Also check homebrew path on macOS.
		for _, candidate := range []string{
			"/opt/homebrew/sbin/mke2fs",
			"/usr/local/sbin/mke2fs",
			"/usr/sbin/mke2fs",
		} {
			if _, err2 := os.Stat(candidate); err2 == nil {
				mke2fs = candidate
				err = nil
				break
			}
		}
	}
	if err != nil {
		t.Skip("mke2fs not available — skipping synthetic image test")
		return ""
	}

	f, err := os.CreateTemp("", fmt.Sprintf("ext4-synth-%dM-*.img", sizeMB))
	if err != nil {
		t.Fatalf("create temp image: %v", err)
	}
	name := f.Name()
	if err := f.Truncate(int64(sizeMB) * 1024 * 1024); err != nil {
		f.Close()
		t.Fatalf("truncate image: %v", err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(name) })

	// Format: ext4 with extents + metadata_csum + inline data disabled.
	cmd := exec.Command(mke2fs,
		"-t", "ext4",
		"-F",
		"-O", "extents,metadata_csum,^inline_data,^huge_file",
		"-E", "lazy_itable_init=0,lazy_journal_init=0",
		"-L", "test",
		name,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mke2fs failed: %v\n%s", err, out)
	}
	return name
}

// copyImage copies src to a new temp file and returns its path.
// The copy is automatically removed when the test ends.
func copyImage(t *testing.T, src string) string {
	t.Helper()
	sf, err := os.Open(src)
	if err != nil {
		t.Fatalf("open source image: %v", err)
	}
	defer sf.Close()

	df, err := os.CreateTemp("", "ext4-copy-*.img")
	if err != nil {
		t.Fatalf("create copy: %v", err)
	}
	name := df.Name()
	t.Cleanup(func() { os.Remove(name) })

	if _, err := io.Copy(df, sf); err != nil {
		df.Close()
		t.Fatalf("copy image: %v", err)
	}
	if err := df.Close(); err != nil {
		t.Fatalf("close copy: %v", err)
	}
	return name
}

// debugfsWrite uses debugfs to write a file into an ext4 image.
func debugfsWrite(t *testing.T, img, srcFile, dstPath string) {
	t.Helper()
	debugfs, err := exec.LookPath("debugfs")
	if err != nil {
		for _, c := range []string{"/opt/homebrew/sbin/debugfs", "/usr/local/sbin/debugfs", "/usr/sbin/debugfs"} {
			if _, err2 := os.Stat(c); err2 == nil {
				debugfs = c
				break
			}
		}
	}
	if debugfs == "" {
		t.Skip("debugfs not available")
		return
	}
	cmd := exec.Command(debugfs, "-w", img, "-R",
		fmt.Sprintf("write %s %s", srcFile, dstPath))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("debugfs write: %v\n%s", err, out)
	}
}
