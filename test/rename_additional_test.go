package filesystem_ext4_test

import (
	"testing"

	ext4 "github.com/go-filesystems/ext4"
)

func TestRename_CrossParent_UpdateDotDot(t *testing.T) {
	path := t.TempDir() + "/img2.img"
	fs, err := ext4.Format(path, 20*1024*1024, ext4.FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()

	if err := fs.MkDir("/src", 0o755); err != nil {
		t.Fatalf("MkDir src: %v", err)
	}
	if err := fs.MkDir("/dst", 0o755); err != nil {
		t.Fatalf("MkDir dst: %v", err)
	}
	if err := fs.MkDir("/src/sub", 0o755); err != nil {
		t.Fatalf("MkDir src/sub: %v", err)
	}

	if err := fs.Rename("/src/sub", "/dst/sub"); err != nil {
		t.Fatalf("Rename cross-parent: %v", err)
	}

	if _, err := fs.Stat("/dst/sub"); err != nil {
		t.Fatalf("Stat /dst/sub: %v", err)
	}
	if _, err := fs.Stat("/src/sub"); err == nil {
		t.Fatalf("expected /src/sub to be gone")
	}

	// ".." is omitted from ListDir, so verify the moved directory's on-disk
	// ".." entry was repointed to the new parent by resolving it through path
	// lookup: "/dst/sub/.." must resolve (via the ".." dirent) to "/dst".
	dotdotStat, err := fs.Stat("/dst/sub/..")
	if err != nil {
		t.Fatalf("Stat /dst/sub/..: %v", err)
	}
	dstStat, err := fs.Stat("/dst")
	if err != nil {
		t.Fatalf("Stat /dst: %v", err)
	}
	if dotdotStat.Inode() != dstStat.Inode() {
		t.Fatalf("updateDotDot failed: '..' inode = %d, want %d", dotdotStat.Inode(), dstStat.Inode())
	}
}

func TestFS_DeleteDir_Recursive(t *testing.T) {
	path := t.TempDir() + "/img3.img"
	fs, err := ext4.Format(path, 20*1024*1024, ext4.FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()

	if err := fs.MkDir("/tree", 0o755); err != nil {
		t.Fatalf("MkDir tree: %v", err)
	}
	if err := fs.MkDir("/tree/sub", 0o755); err != nil {
		t.Fatalf("MkDir tree/sub: %v", err)
	}
	if err := fs.WriteFile("/tree/sub/file.txt", []byte("data"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := fs.DeleteDir("/tree"); err != nil {
		t.Fatalf("DeleteDir: %v", err)
	}
	if _, err := fs.Stat("/tree"); err == nil {
		t.Fatalf("expected /tree to be gone after DeleteDir")
	}
}

func TestWriteFile_OverwriteAllocFree(t *testing.T) {
	path := t.TempDir() + "/img4.img"
	fs, err := ext4.Format(path, 20*1024*1024, ext4.FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()

	big := make([]byte, 8192)
	for i := range big {
		big[i] = byte(i)
	}
	if err := fs.WriteFile("/big.bin", big, 0o644); err != nil {
		t.Fatalf("WriteFile big: %v", err)
	}

	small := []byte("small")
	if err := fs.WriteFile("/big.bin", small, 0o644); err != nil {
		t.Fatalf("WriteFile overwrite: %v", err)
	}
	got, err := fs.ReadFile("/big.bin")
	if err != nil {
		t.Fatalf("ReadFile after overwrite: %v", err)
	}
	if string(got) != string(small) {
		t.Fatalf("overwrite content mismatch: got %q want %q", got, small)
	}
}
