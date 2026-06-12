package filesystem_ext4_test

import (
	"fmt"
	"strings"
	"testing"

	ext4 "github.com/go-filesystems/ext4"
)

// TestDeepDirectoryPathResolves builds a directory tree far deeper than the
// historical 8-component limit and confirms a file at the bottom can be
// written and read back. This is a regression test for the symlink-loop guard
// in lookupPathFrom, which previously incremented on every directory-component
// descent and rejected any path deeper than 8 components as a "symlink loop".
func TestDeepDirectoryPathResolves(t *testing.T) {
	fs, cleanup := ext4.NewTempFS(t)
	defer cleanup()

	const depth = 20 // well beyond the old 8-component cap
	var sb strings.Builder
	for i := 0; i < depth; i++ {
		sb.WriteString(fmt.Sprintf("/d%d", i))
		if err := fs.MkDir(sb.String(), 0o755); err != nil {
			t.Fatalf("MkDir %q: %v", sb.String(), err)
		}
	}

	deepFile := sb.String() + "/deep.txt"
	want := []byte("reached the bottom of a 20-level tree\n")
	if err := fs.WriteFile(deepFile, want, 0o644); err != nil {
		t.Fatalf("WriteFile %q: %v", deepFile, err)
	}

	got, err := fs.ReadFile(deepFile)
	if err != nil {
		t.Fatalf("ReadFile %q: %v", deepFile, err)
	}
	if string(got) != string(want) {
		t.Fatalf("ReadFile %q = %q, want %q", deepFile, got, want)
	}

	// A path far deeper than the symlink-hop bound (40) must still resolve,
	// since plain directory descent does not count as a symlink hop.
	fs2, cleanup2 := ext4.NewTempFS(t)
	defer cleanup2()
	var sb2 strings.Builder
	const veryDeep = 60
	for i := 0; i < veryDeep; i++ {
		sb2.WriteString(fmt.Sprintf("/n%d", i))
		if err := fs2.MkDir(sb2.String(), 0o755); err != nil {
			t.Fatalf("MkDir %q: %v", sb2.String(), err)
		}
	}
	leaf := sb2.String() + "/leaf"
	if err := fs2.WriteFile(leaf, []byte("ok"), 0o644); err != nil {
		t.Fatalf("WriteFile %q: %v", leaf, err)
	}
	if got, err := fs2.ReadFile(leaf); err != nil || string(got) != "ok" {
		t.Fatalf("ReadFile %q = %q, err=%v; want %q", leaf, got, err, "ok")
	}
}
