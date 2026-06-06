package filesystem_ext4_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	ext4 "github.com/go-filesystems/ext4"
	filesystem "github.com/go-filesystems/interface"
)

func TestInspectDebianHostname(t *testing.T) {
	path := filepath.Join(os.Getenv("HOME"), ".mock", "cache", "https____cloud.debian.org_images_cloud_trixie_latest_debian-13-genericcloud-arm64.raw", "debian-13-genericcloud-arm64.raw")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("debian image not found at %s", path)
	}
	// Use a writable copy to avoid altering the cached base image.
	path = copyImage(t, path)
	var fs filesystem.Filesystem
	var efs filesystem.Filesystem
	var err error
	// Try explicit partition indices  -1..7 like the stress test.
	for i := -1; i < 8; i++ {
		efs, err = ext4.Open(path, i)
		if err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	fs = efs
	defer fs.Close()
	data, err := fs.ReadFile("/etc/hosts")
	if err != nil {
		t.Fatalf("ReadFile /etc/hosts: %v", err)
	}
	fmt.Printf("/etc/hosts len=%d content=%q\n", len(data), string(data))
}
