package filesystem_ext4

import (
	"path/filepath"
	"testing"
)

// Read-path benchmarks on a real mke2fs metadata_csum image (the formatter in
// this package does not emit metadata_csum, so BenchmarkListDir/BenchmarkStat
// never exercise checksum verification). Run with -benchmem: allocation counts
// are exact under load, times are not.
func openCsumFixture(b *testing.B) (fsys interface {
	Close() error
}, path string) {
	b.Helper()
	path = filepath.Join(fixtureDir(b), "ext4_4k.img")
	return nil, path
}

func BenchmarkCsumListDir(b *testing.B) {
	_, path := openCsumFixture(b)
	fsys, err := Open(path, -1)
	if err != nil {
		b.Fatal(err)
	}
	defer fsys.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := fsys.ListDir("/"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCsumStat(b *testing.B) {
	_, path := openCsumFixture(b)
	fsys, err := Open(path, -1)
	if err != nil {
		b.Fatal(err)
	}
	defer fsys.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := fsys.Stat("/fast.lnk"); err != nil {
			b.Fatal(err)
		}
	}
}
