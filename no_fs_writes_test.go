package filesystem_ext4

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The library must never write outside the image it was given: a fixed or
// predictable path in a shared directory is open to symlink and pre-creation
// attacks, leaks volume contents and grows without bound (v0.7.1 - v0.7.3).
func TestLibrarySourceNamesNoTempPath(t *testing.T) {
	forbidden := []string{`"/tmp`, `"/var/tmp`, "os.TempDir(", "os.CreateTemp(", "os.MkdirTemp(", "ioutil.TempFile(", "ioutil.TempDir("}
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != "." && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, bad := range forbidden {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s contains %s: library code must not name a temporary path", p, bad)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func listPrefixed(t *testing.T, dir, prefix string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, prefix+"*"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// A full write/delete workload, with every diagnostic switch on, creates no
// file under /tmp and none under TMPDIR.
func TestWorkloadWritesNothingOutsideTheImage(t *testing.T) {
	watch := filepath.Join(t.TempDir(), "tmpdir")
	if err := os.Mkdir(watch, 0o700); err != nil {
		t.Fatal(err)
	}
	before := listPrefixed(t, "/tmp", "ext4_")

	t.Setenv("TMPDIR", watch)
	t.Setenv("EXT4_DUMP_TX_BLOCKS", "1")
	t.Setenv("EXT4_DUMP_DIRBLOCKS", "1")
	t.Setenv("EXT4_FOCUS_SEQ", "1")
	t.Setenv("EXT4_LOCK_DEBUG", "1")

	fsys, cleanup := NewTempFS(t)
	defer cleanup()
	data := pattern(3<<20, 3)
	if err := fsys.WriteFile("/big", data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fsys.MkDir("/d", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile("/d/f", pattern(5000, 1), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fsys.DeleteFile("/big"); err != nil {
		t.Fatal(err)
	}
	if err := fsys.DeleteDir("/d"); err != nil {
		t.Fatal(err)
	}

	after := listPrefixed(t, "/tmp", "ext4_")
	seen := map[string]bool{}
	for _, p := range before {
		seen[p] = true
	}
	for _, p := range after {
		if !seen[p] {
			t.Errorf("the workload created %s", p)
		}
	}
	ents, err := os.ReadDir(watch)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		t.Errorf("the workload created %s in TMPDIR", e.Name())
	}
}
