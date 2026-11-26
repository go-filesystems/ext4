package filesystem_ext4

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func benchImage(b *testing.B) string {
	b.Helper()
	mke2fs := hostTool("mke2fs")
	if mke2fs == "" {
		b.Skip("mke2fs not available")
	}
	img := filepath.Join(b.TempDir(), "img")
	if out, err := exec.Command(mke2fs, "-q", "-t", "ext4", "-O", "metadata_csum,uninit_bg", "-b", "4096", img, "64M").CombinedOutput(); err != nil {
		b.Skipf("mke2fs unusable here: %v\n%s", err, out)
	}
	return img
}

// BenchmarkDeleteFile measures one create+delete of a 3-block file; read the
// allocs/op column (allocation counts are exact, times depend on load).
func BenchmarkDeleteFile(b *testing.B) {
	fsys, err := Open(benchImage(b), -1)
	if err != nil {
		b.Fatal(err)
	}
	defer fsys.Close()
	data := pattern(10000, 1)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := fsys.WriteFile("/f", data, 0o644); err != nil {
			b.Fatal(err)
		}
		if err := fsys.DeleteFile("/f"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkDeleteDir measures one MkDir+file+DeleteDir.
func BenchmarkDeleteDir(b *testing.B) {
	fsys, err := Open(benchImage(b), -1)
	if err != nil {
		b.Fatal(err)
	}
	defer fsys.Close()
	data := pattern(100, 1)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := fsys.MkDir("/d", 0o755); err != nil {
			b.Fatal(err)
		}
		if err := fsys.WriteFile("/d/f", data, 0o644); err != nil {
			b.Fatal(err)
		}
		if err := fsys.DeleteDir("/d"); err != nil {
			b.Fatal(err)
		}
	}
}
