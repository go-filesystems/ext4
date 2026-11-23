package filesystem_ext4

import (
	"os"
	"path/filepath"
	"testing"
)

// Extent-tree benchmarks on a metadata_csum image. The file has 9 separate
// extents, so its tree needs an extent block below the inode: reading it
// verifies et_checksum, and growing/truncating it re-stamps the block.
// Self-contained (no helper from csum_extent_test.go) so the same file can be
// dropped into an older checkout for a before/after allocation comparison.
func extentBenchFS(b *testing.B) *ext4FS {
	b.Helper()
	src, err := os.ReadFile(filepath.Join(fixtureDir(b), "ext4_4k.img"))
	if err != nil {
		b.Fatal(err)
	}
	img := filepath.Join(b.TempDir(), "img")
	if err := os.WriteFile(img, src, 0o600); err != nil {
		b.Fatal(err)
	}
	fsys, err := Open(img, -1)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { fsys.Close() })
	if err := fsys.WriteFile("/bench.bin", nil, 0o644); err != nil {
		b.Fatal(err)
	}
	f, err := fsys.(*ext4FS).OpenFile("/bench.bin")
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 9; i++ {
		if _, err := f.(interface {
			WriteAt([]byte, int64) (int, error)
		}).WriteAt([]byte("island"), int64(i)*3*4096); err != nil {
			b.Fatal(err)
		}
	}
	f.Close()
	return fsys.(*ext4FS)
}

func BenchmarkCsumExtentTreeRead(b *testing.B) {
	fsys := extentBenchFS(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := fsys.ReadFile("/bench.bin"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCsumExtentTreeRewrite(b *testing.B) {
	fsys := extentBenchFS(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f, err := fsys.OpenFile("/bench.bin")
		if err != nil {
			b.Fatal(err)
		}
		w := f.(interface {
			WriteAt([]byte, int64) (int, error)
			Truncate(int64) error
		})
		if _, err := w.WriteAt([]byte("grow"), 10*3*4096); err != nil { // new extent: leaf block rewritten
			b.Fatal(err)
		}
		if err := w.Truncate(9*3*4096 + 6); err != nil { // and again
			b.Fatal(err)
		}
		f.Close()
	}
}
