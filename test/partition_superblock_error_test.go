package filesystem_ext4_test

import (
	"encoding/binary"
	"errors"
	"testing"

	ext4 "github.com/go-filesystems/ext4"
)

func TestPartitionAndSuperblockErrorPaths(t *testing.T) {
	errBoom := errors.New("boom")

	if _, err := ext4.PartitionOffset(&errRW{readErr: errBoom}, -1); err == nil {
		t.Fatalf("expected partition header read error")
	}
	if _, err := ext4.ReadSuperblock(&errRW{readErr: errBoom}, 0); err == nil {
		t.Fatalf("expected superblock read error")
	}

	t.Run("gpt skips empty entry and honors explicit index", func(t *testing.T) {
		le := binary.LittleEndian
		buf := make([]byte, 64*1024)
		hdr := buf[512:]
		copy(hdr[:8], "EFI PART")
		le.PutUint64(hdr[72:], 2)
		le.PutUint32(hdr[80:], 2)
		le.PutUint32(hdr[84:], 128)

		entry2 := buf[1152:1280]
		copy(entry2[:16], ext4.LinuxPartTypeGPT[:])
		le.PutUint64(entry2[32:], 4096)

		off, err := ext4.PartitionOffset(memReaderAt(buf), 1)
		if err != nil {
			t.Fatalf("PartitionOffset explicit index: %v", err)
		}
		if want := int64(4096 * ext4.SectorSize); off != want {
			t.Fatalf("offset = %d, want %d", off, want)
		}
	})

	t.Run("mbr skips zero-start entry", func(t *testing.T) {
		le := binary.LittleEndian
		buf := make([]byte, 4096)
		buf[510] = 0x55
		buf[511] = 0xAA

		entry1 := buf[446:462]
		entry1[4] = 0x83
		le.PutUint32(entry1[8:], 0)

		entry2 := buf[462:478]
		entry2[4] = 0x83
		le.PutUint32(entry2[8:], 2048)

		off, err := ext4.PartitionOffset(memReaderAt(buf), -1)
		if err != nil {
			t.Fatalf("PartitionOffset MBR: %v", err)
		}
		if want := int64(2048 * ext4.SectorSize); off != want {
			t.Fatalf("offset = %d, want %d", off, want)
		}
	})
}
