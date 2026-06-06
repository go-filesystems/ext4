package filesystem_ext4_test

import (
	"strings"
	"testing"

	ext4 "github.com/go-filesystems/ext4"
)

func TestOpen(t *testing.T) {
	raw := rockyRawPath(t)
	idx := rockyExt4PartIdx(t, raw)
	// Open a writable copy to avoid altering the cached raw image.
	tmp := copyImage(t, raw)
	fs, err := ext4.Open(tmp, idx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()
}

func TestListDir_Root(t *testing.T) {
	raw := rockyRawPath(t)
	idx := rockyExt4PartIdx(t, raw)
	// Open a writable copy to avoid altering the cached raw image.
	tmp := copyImage(t, raw)
	fs, err := ext4.Open(tmp, idx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir /: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("root directory is empty")
	}
	names := make(map[string]bool, len(entries))
	for _, e := range entries {
		names[e.Name()] = true
	}
	// /boot partition: grub2 or loader; root partition: etc or usr.
	found := names["grub2"] || names["grub"] || names["loader"] ||
		names["etc"] || names["usr"] || names["bin"]
	if !found {
		var ns []string
		for _, e := range entries {
			ns = append(ns, e.Name())
		}
		t.Errorf("unexpected root directory contents: %v", ns)
	}
}

func TestReadFile_EtcHostname(t *testing.T) {
	raw := rockyRawPath(t)
	idx := rockyExt4PartIdx(t, raw)
	// Open a writable copy to avoid altering the cached raw image.
	tmp := copyImage(t, raw)
	fs, err := ext4.Open(tmp, idx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	// Try files present on /boot (grub.cfg) and on / (hosts).
	for _, path := range []string{"/grub2/grub.cfg", "/etc/hosts"} {
		data, err := fs.ReadFile(path)
		if err != nil {
			continue
		}
		if len(data) == 0 {
			t.Errorf("%s is empty", path)
		}
		t.Logf("read %s (%d bytes)", path, len(data))
		return
	}
	t.Skip("neither /grub2/grub.cfg nor /etc/hosts found in ext4 partition")
}

func TestReadFile_EtcOSRelease(t *testing.T) {
	raw := rockyRawPath(t)
	idx := rockyExt4PartIdx(t, raw)
	// Open a writable copy to avoid altering the cached raw image.
	tmp := copyImage(t, raw)
	fs, err := ext4.Open(tmp, idx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	data, err := fs.ReadFile("/etc/os-release")
	if err != nil {
		// /boot partition does not carry os-release — acceptable.
		t.Skip("/etc/os-release not present in ext4 partition (likely /boot)")
		return
	}
	content := string(data)
	if !strings.Contains(content, "Rocky") && !strings.Contains(content, "rocky") {
		t.Errorf("expected Rocky Linux in /etc/os-release, got:\n%s", content)
	}
}

func TestWriteFile_RoundTrip(t *testing.T) {
	raw := rockyRawPath(t)
	idx := rockyExt4PartIdx(t, raw)

	// Copy the raw image so we never modify the cached conversion.
	tmp := copyImage(t, raw)

	const testPath = "/mock-ext4-test.txt"
	const testContent = "mock ext4 write-round-trip test\n"

	fs, err := ext4.Open(tmp, idx)
	if err != nil {
		t.Fatalf("Open for write: %v", err)
	}
	if err := fs.WriteFile(testPath, []byte(testContent), 0o644); err != nil {
		fs.Close()
		t.Fatalf("WriteFile: %v", err)
	}
	fs.Close()

	fs2, err := ext4.Open(tmp, idx)
	if err != nil {
		t.Fatalf("Open for readback: %v", err)
	}
	defer fs2.Close()

	got, err := fs2.ReadFile(testPath)
	if err != nil {
		t.Fatalf("ReadFile after write: %v", err)
	}
	if string(got) != testContent {
		t.Errorf("round-trip mismatch:\n  want %q\n  got  %q", testContent, got)
	}
}
