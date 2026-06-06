package filesystem_ext4_test

import (
	"testing"

	ext4 "github.com/go-filesystems/ext4"
)

func TestFS_MkDir_Delete_Rename_DeleteFile_ReadLink(t *testing.T) {
	fs, cleanup := ext4.NewTempFS(t)
	defer cleanup()

	// MkDir
	if err := fs.MkDir("/adir", 0o755); err != nil {
		t.Fatalf("MkDir: %v", err)
	}
	if _, err := fs.Stat("/adir"); err != nil {
		t.Fatalf("Stat after MkDir: %v", err)
	}

	// Write a file inside and delete it.
	if err := fs.WriteFile("/adir/file.txt", []byte("data"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := fs.DeleteFile("/adir/file.txt"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}

	// Rename directory
	if err := fs.Rename("/adir", "/bdir"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if _, err := fs.Stat("/bdir"); err != nil {
		t.Fatalf("Stat after Rename: %v", err)
	}

	// Create a symlink via exported test helper.
	target := "target-path"
	ext4.AddFastSymlink(t, fs, "/", "mylink", target)

	// ReadLink: verify inline symlink handling via public API.
	got, err := fs.ReadLink("/mylink")
	if err != nil {
		t.Fatalf("ReadLink: %v", err)
	}
	if got != target {
		t.Fatalf("ReadLink: got %q want %q", got, target)
	}
}
