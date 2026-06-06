package filesystem_ext4_test

import (
	"bytes"
	"encoding/binary"
	"os"
	"testing"

	ext4 "github.com/go-filesystems/ext4"
)

const (
	journalMagic uint32 = 0x4A424432
	commitMagic  uint32 = 0x434D4954
)

// Test that ReplayOnOpen applies committed transactions found in the
// sidecar journal to the backing image.
func TestJournalReplay_AppliesCommittedTx(t *testing.T) {
	tmp, err := os.CreateTemp("", "ext4img.*")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer os.Remove(tmp.Name())
	jsPath := tmp.Name() + ".journal"
	defer os.Remove(jsPath)

	// Prepare a superblock stub.
	sb := ext4.NewTestSuperblock(16, 128, 8, 4096)

	// Ensure the target block (5) is initially zeroed.
	blk := uint64(5)
	zero := make([]byte, int(sb.BlockSize))
	if _, err := tmp.WriteAt(zero, int64(blk)*int64(sb.BlockSize)); err != nil {
		t.Fatalf("write initial: %v", err)
	}

	// Construct a committed tx in the sidecar journal that updates block 5.
	data := bytes.Repeat([]byte{0xAB}, int(sb.BlockSize))
	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.LittleEndian, journalMagic); err != nil {
		t.Fatalf("encode hdr: %v", err)
	}
	seq := uint64(1)
	if err := binary.Write(&buf, binary.LittleEndian, seq); err != nil {
		t.Fatalf("encode seq: %v", err)
	}
	nDesc := uint32(1)
	if err := binary.Write(&buf, binary.LittleEndian, nDesc); err != nil {
		t.Fatalf("encode nDesc: %v", err)
	}
	if err := binary.Write(&buf, binary.LittleEndian, blk); err != nil {
		t.Fatalf("encode desc blk: %v", err)
	}
	if err := binary.Write(&buf, binary.LittleEndian, uint32(len(data))); err != nil {
		t.Fatalf("encode desc ln: %v", err)
	}
	if _, err := buf.Write(data); err != nil {
		t.Fatalf("write data: %v", err)
	}
	crc := ext4.CRC32c(0, buf.Bytes())

	js, err := os.OpenFile(jsPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("open js: %v", err)
	}
	if _, err := js.Write(buf.Bytes()); err != nil {
		t.Fatalf("write js: %v", err)
	}
	if err := binary.Write(js, binary.LittleEndian, commitMagic); err != nil {
		t.Fatalf("write commit magic: %v", err)
	}
	if err := binary.Write(js, binary.LittleEndian, seq); err != nil {
		t.Fatalf("write commit seq: %v", err)
	}
	if err := binary.Write(js, binary.LittleEndian, crc); err != nil {
		t.Fatalf("write commit crc: %v", err)
	}
	js.Close()

	// Open a Journal instance and replay. OpenJournal should find the sidecar and
	// enable the journal for tmp.Name().
	j, err := ext4.OpenJournal(tmp, 0, int(sb.BlockSize), sb)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	if err := j.ReplayOnOpen(); err != nil {
		t.Fatalf("ReplayOnOpen: %v", err)
	}

	// Verify the block was applied to the backing file.
	got := make([]byte, int(sb.BlockSize))
	if _, err := tmp.ReadAt(got, int64(blk)*int64(sb.BlockSize)); err != nil {
		t.Fatalf("read applied block: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("block mismatch after replay")
	}
}

func TestJournalReplay_IgnoresUncommittedTx(t *testing.T) {
	tmp, err := os.CreateTemp("", "ext4img.*")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer os.Remove(tmp.Name())
	jsPath := tmp.Name() + ".journal"
	defer os.Remove(jsPath)

	sb := ext4.NewTestSuperblock(16, 128, 8, 4096)
	blk := uint64(6)
	zero := make([]byte, int(sb.BlockSize))
	if _, err := tmp.WriteAt(zero, int64(blk)*int64(sb.BlockSize)); err != nil {
		t.Fatalf("write initial: %v", err)
	}

	// Write a tx header+desc+data but omit the commit record (simulates crash).
	data := bytes.Repeat([]byte{0xCD}, int(sb.BlockSize))
	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.LittleEndian, journalMagic); err != nil {
		t.Fatalf("encode hdr: %v", err)
	}
	seq := uint64(1)
	if err := binary.Write(&buf, binary.LittleEndian, seq); err != nil {
		t.Fatalf("encode seq: %v", err)
	}
	nDesc := uint32(1)
	if err := binary.Write(&buf, binary.LittleEndian, nDesc); err != nil {
		t.Fatalf("encode nDesc: %v", err)
	}
	if err := binary.Write(&buf, binary.LittleEndian, blk); err != nil {
		t.Fatalf("encode desc blk: %v", err)
	}
	if err := binary.Write(&buf, binary.LittleEndian, uint32(len(data))); err != nil {
		t.Fatalf("encode desc ln: %v", err)
	}
	if _, err := buf.Write(data); err != nil {
		t.Fatalf("write data: %v", err)
	}

	if err := os.WriteFile(jsPath, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write js: %v", err)
	}

	j, err := ext4.OpenJournal(tmp, 0, int(sb.BlockSize), sb)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	if err := j.ReplayOnOpen(); err != nil {
		t.Fatalf("ReplayOnOpen: %v", err)
	}

	got := make([]byte, int(sb.BlockSize))
	if _, err := tmp.ReadAt(got, int64(blk)*int64(sb.BlockSize)); err != nil {
		t.Fatalf("read applied block: %v", err)
	}
	if bytes.Equal(got, data) {
		t.Fatalf("expected uncommitted tx to be ignored")
	}
}

func TestJournalReplay_IgnoresBadCRC(t *testing.T) {
	tmp, err := os.CreateTemp("", "ext4img.*")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer os.Remove(tmp.Name())
	jsPath := tmp.Name() + ".journal"
	defer os.Remove(jsPath)

	sb := ext4.NewTestSuperblock(16, 128, 8, 4096)
	blk := uint64(7)
	zero := make([]byte, int(sb.BlockSize))
	if _, err := tmp.WriteAt(zero, int64(blk)*int64(sb.BlockSize)); err != nil {
		t.Fatalf("write initial: %v", err)
	}

	// Commit a tx but write an incorrect CRC to simulate corruption.
	data := bytes.Repeat([]byte{0xEF}, int(sb.BlockSize))
	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.LittleEndian, journalMagic); err != nil {
		t.Fatalf("encode hdr: %v", err)
	}
	seq := uint64(1)
	if err := binary.Write(&buf, binary.LittleEndian, seq); err != nil {
		t.Fatalf("encode seq: %v", err)
	}
	nDesc := uint32(1)
	if err := binary.Write(&buf, binary.LittleEndian, nDesc); err != nil {
		t.Fatalf("encode nDesc: %v", err)
	}
	if err := binary.Write(&buf, binary.LittleEndian, blk); err != nil {
		t.Fatalf("encode desc blk: %v", err)
	}
	if err := binary.Write(&buf, binary.LittleEndian, uint32(len(data))); err != nil {
		t.Fatalf("encode desc ln: %v", err)
	}
	if _, err := buf.Write(data); err != nil {
		t.Fatalf("write data: %v", err)
	}
	// Intentionally produce a wrong CRC value
	wrongCRC := uint32(0xDEADBEEF)

	js, err := os.OpenFile(jsPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("open js: %v", err)
	}
	if _, err := js.Write(buf.Bytes()); err != nil {
		t.Fatalf("write js: %v", err)
	}
	if err := binary.Write(js, binary.LittleEndian, commitMagic); err != nil {
		t.Fatalf("write commit magic: %v", err)
	}
	if err := binary.Write(js, binary.LittleEndian, seq); err != nil {
		t.Fatalf("write commit seq: %v", err)
	}
	if err := binary.Write(js, binary.LittleEndian, wrongCRC); err != nil {
		t.Fatalf("write commit crc: %v", err)
	}
	js.Close()

	j, err := ext4.OpenJournal(tmp, 0, int(sb.BlockSize), sb)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	if err := j.ReplayOnOpen(); err != nil {
		t.Fatalf("ReplayOnOpen: %v", err)
	}

	got := make([]byte, int(sb.BlockSize))
	if _, err := tmp.ReadAt(got, int64(blk)*int64(sb.BlockSize)); err != nil {
		t.Fatalf("read applied block: %v", err)
	}
	if bytes.Equal(got, data) {
		t.Fatalf("expected bad-CRC tx to be ignored")
	}
}
