// SPDX-License-Identifier: BSD-3-Clause

package filesystem_ext4

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestDeleteFileReportsErrorsThatAreNotAMissingPath.
//
// DeleteFile is documented idempotent: deleting a path that is not there
// returns nil. That is a design choice and this test does not touch it.
//
// ⛔ What it did was wider than what it said. removeFile discarded the error
// from lookupParent and from lookupPath ENTIRELY:
//
//	parent, name, err := lookupParent(...)
//	if err != nil {
//		return nil // parent not found — already gone, idempotent
//	}
//
// The comment says "not found". The code says "anything". So a malformed path,
// or a lookup that failed because the image is broken, came back as a
// SUCCESSFUL delete -- the caller is told the file is gone, and nothing was
// touched.
//
// The witness needs no corruption: a path ending in "/" is refused by
// lookupParent with "path must not end with /", which is a usage error and
// never meant "already gone".
func TestDeleteFileReportsErrorsThatAreNotAMissingPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fs.img")
	fsys, err := Format(path, 4096*4096, FormatConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer fsys.Close()

	if err := fsys.WriteFile("/a.txt", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// removeFile and removeDir had the same two lines each. Fixing one of
	// four instances of one defect is worse than leaving all four, because
	// the next reader sees a handled case and assumes the rest are too.
	for _, tc := range []struct {
		what, path string
		del        func(string) error
	}{
		{"a path ending in a slash", "/a.txt/", fsys.DeleteFile},
		{"an empty path", "", fsys.DeleteFile},
		{"a path ending in a slash", "/a.txt/", fsys.DeleteDir},
		{"an empty path", "", fsys.DeleteDir},
	} {
		if err := tc.del(tc.path); err == nil {
			t.Errorf("deleting %q: %s reported success; nothing was deleted and the caller is told otherwise",
				tc.path, tc.what)
		}
	}

	// And the documented behaviour still holds: a path that is simply not
	// there is not an error.
	if err := fsys.DeleteFile("/nope.txt"); err != nil {
		t.Errorf("DeleteFile on a missing path must stay idempotent, got %v", err)
	}
	if err := fsys.DeleteDir("/nodir"); err != nil {
		t.Errorf("DeleteDir on a missing path must stay idempotent, got %v", err)
	}

	// The file that does exist is still deletable, and still there until then.
	if _, err := fsys.Stat("/a.txt"); err != nil {
		t.Fatalf("/a.txt should be untouched by the refusals above: %v", err)
	}
	if err := fsys.DeleteFile("/a.txt"); err != nil {
		t.Fatalf("deleting a real file must work: %v", err)
	}
	if _, err := fsys.Stat("/a.txt"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("after DeleteFile the path must be gone, got %v", err)
	}
}
