package filesystem_ext4_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	ext4 "github.com/go-filesystems/ext4"
)

// findDebugfs locates a debugfs binary the same way findMke2fs locates mke2fs.
// It returns "" (and skips the test) if none is found.
func findDebugfs(t *testing.T) string {
	t.Helper()
	if p, err := exec.LookPath("debugfs"); err == nil {
		return p
	}
	for _, c := range []string{
		"/usr/sbin/debugfs",
		"/sbin/debugfs",
		"/usr/local/sbin/debugfs",
		"/opt/homebrew/sbin/debugfs",
	} {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	t.Skip("debugfs not available — skipping inline_data debugfs cross-check")
	return ""
}

// debugfsListDir runs `debugfs -R "ls -l <ino>" <img>` and returns a map of
// name -> inode number for every entry it reports (including "." and "..").
// The `ls -l` output columns are: inode, mode, filetype, uid, gid, size,
// date(3 fields), name. The trailing fields are the name, which we join.
func debugfsListDir(t *testing.T, debugfs, img string, ino uint32) map[string]uint32 {
	t.Helper()
	cmd := exec.Command(debugfs, "-R", "ls -l <"+strconv.Itoa(int(ino))+">", img)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("debugfs ls -l <%d>: %v\n%s", ino, err, out)
	}
	// Each entry line has the form:
	//   <inode> <mode> (<ftype>) <uid> <gid> <size> <date> <time> <name>
	// e.g. "    14   40775 (0)   1000   1000      60 12-Jun-2026 19:10 .".
	// That is 9 whitespace-separated fields, with the name at index 8.
	res := map[string]uint32{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 9 {
			continue
		}
		n, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		// The name is everything from field index 8 onward (rejoined to tolerate
		// names with spaces; the fixture names here contain none).
		name := strings.Join(fields[8:], " ")
		if name == "" {
			continue
		}
		res[name] = uint32(n)
	}
	return res
}

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

// TestInlineDir_RealImage_MatchesDebugfs formats an ext4 image with inline_data
// enabled, creates an inline directory holding several named entries, and
// asserts that ListDir on that directory returns exactly the named entries
// (ListDir omits "." and "..") with the inode numbers debugfs reports for the
// same directory.
//
// This is the authoritative cross-check for the inline-directory parser: the
// real kernel layout stores the parent inode in i_block[0:4], synthesises "."
// from the inode's own number, and starts the named-entry stream at offset 4.
func TestInlineDir_RealImage_MatchesDebugfs(t *testing.T) {
	mke2fs := findMke2fs(t)
	debugfs := findDebugfs(t)

	// Build a source tree whose subdirectory holds several small files so the
	// directory itself is stored inline (well under one block of entries).
	src := t.TempDir()
	sub := filepath.Join(src, "inlinedir")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("mkdir inlinedir: %v", err)
	}
	for _, name := range []string{"alpha", "beta", "gamma"} {
		if err := os.WriteFile(filepath.Join(sub, name), []byte(name+"\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	img := filepath.Join(t.TempDir(), "inlinedir.img")
	f, err := os.Create(img)
	if err != nil {
		t.Fatalf("create image: %v", err)
	}
	if err := f.Truncate(8 * 1024 * 1024); err != nil {
		f.Close()
		t.Fatalf("truncate: %v", err)
	}
	f.Close()

	cmd := exec.Command(mke2fs,
		"-t", "ext4",
		"-F",
		"-b", "1024",
		"-O", "inline_data,extents,metadata_csum",
		"-E", "lazy_itable_init=0,lazy_journal_init=0",
		"-d", src,
		"-L", "inlinedir",
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

	// Resolve the inode number of the inline directory so we can ask debugfs
	// about the very same object.
	st, err := fs.Stat("/inlinedir")
	if err != nil {
		t.Fatalf("Stat /inlinedir: %v", err)
	}
	dirIno := uint32(st.Inode())

	// Our parser's view.
	entries, err := fs.ListDir("/inlinedir")
	if err != nil {
		t.Fatalf("ListDir /inlinedir: %v", err)
	}
	got := map[string]uint32{}
	for _, e := range entries {
		got[e.Name()] = uint32(e.Inode())
	}

	// debugfs's view of the same inode.
	want := debugfsListDir(t, debugfs, img, dirIno)

	// The directory must really be inline for this test to be meaningful.
	statOut, _ := exec.Command(debugfs, "-R", "stat <"+strconv.Itoa(int(dirIno))+">", img).CombinedOutput()
	if !strings.Contains(string(statOut), "system.data") {
		t.Skipf("/inlinedir (inode %d) is not inline; cannot validate inline-dir parser:\n%s", dirIno, statOut)
	}

	// Sanity: debugfs always reports ".", ".." plus our three names. Verify
	// debugfs really saw the inline directory we created before dropping the
	// dot entries below.
	for _, name := range []string{".", "..", "alpha", "beta", "gamma"} {
		if _, ok := want[name]; !ok {
			t.Fatalf("debugfs listing of inode %d missing %q; got %v", dirIno, name, want)
		}
	}

	// ListDir deliberately omits "." and ".." (see commit "ext4: omit '.' and
	// '..' from ListDir"), whereas debugfs reports them. Drop the dot entries
	// from the expected set so the two views are compared on equal footing.
	delete(want, ".")
	delete(want, "..")

	// ListDir must match debugfs exactly (minus the dot entries): same names,
	// same inode numbers.
	if len(got) != len(want) {
		t.Fatalf("entry count mismatch: ListDir=%v debugfs=%v", got, want)
	}
	for name, wantIno := range want {
		gotIno, ok := got[name]
		if !ok {
			t.Fatalf("ListDir missing entry %q present in debugfs (debugfs=%v, ListDir=%v)", name, want, got)
		}
		if gotIno != wantIno {
			t.Fatalf("entry %q: ListDir inode %d, debugfs inode %d", name, gotIno, wantIno)
		}
	}

	// ListDir must not surface the dot entries.
	if _, ok := got["."]; ok {
		t.Fatalf(`ListDir unexpectedly returned "." (= inode %d)`, got["."])
	}
	if _, ok := got[".."]; ok {
		t.Fatalf(`ListDir unexpectedly returned ".." (= inode %d)`, got[".."])
	}

	// Keep the result deterministic in failure messages.
	names := make([]string, 0, len(got))
	for n := range got {
		names = append(names, n)
	}
	sort.Strings(names)
	t.Logf("inline dir inode %d entries match debugfs: %v", dirIno, names)
}
