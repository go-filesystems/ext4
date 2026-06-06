package filesystem_ext4_test

import (
	"fmt"
	"testing"

	ext4 "github.com/go-filesystems/ext4"
)

// TestWriteFile_ImmediateReadAtomic_UbuntuEtc isolates the failing /etc path
// from write_hammer without concurrent workers.
func TestWriteFile_ImmediateReadAtomic_UbuntuEtc(t *testing.T) {
	src := resolveStressImage(t, allStressImages[1])
	if src == "" {
		t.Skip("no Ubuntu image found in cache")
	}
	raw := toStressRaw(t, src, allStressImages[1].format)
	partIdx := findExt4Partition(t, raw)

	fs, err := ext4.Open(copyStressImage(t, raw), partIdx)
	if err != nil {
		t.Fatalf("ext4.Open: %v", err)
	}
	defer fs.Close()

	for i := 0; i < 500; i++ {
		path := fmt.Sprintf("/etc/ext4hammer-w3-i%06d.txt", i)
		want := fmt.Appendf(nil, "w=%d i=%d\n", 3, i)
		if err := fs.WriteFile(path, want, 0o644); err != nil {
			t.Fatalf("WriteFile(%q): %v", path, err)
		}
		got, err := fs.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%q) after write: %v", path, err)
		}
		if string(got) != string(want) {
			t.Fatalf("ReadFile(%q): got %q, want %q", path, got, want)
		}
	}
}
