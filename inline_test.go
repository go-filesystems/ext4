package filesystem_ext4

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// buildInlineInode builds a minimal 256-byte inode with EXT4_INLINE_DATA_FL
// set, storing the first up to 60 bytes in i_block and, when the data is
// larger, the remainder in an in-inode "system.data" xattr.
func buildInlineInode(t *testing.T, mode uint16, data []byte) []byte {
	t.Helper()
	le := binary.LittleEndian
	raw := make([]byte, 256)
	le.PutUint16(raw[inodeOffMode:], mode)
	le.PutUint32(raw[inodeOffFlags:], InodeFlagInlineData)
	le.PutUint32(raw[inodeOffSizeLo:], uint32(len(data)))

	// i_block prefix.
	n := len(data)
	if n > inlineIBlockMax {
		n = inlineIBlockMax
	}
	copy(raw[inodeOffBlock:inodeOffBlock+n], data[:n])

	if len(data) <= inlineIBlockMax {
		return raw
	}

	// Overflow -> in-inode system.data xattr.
	overflow := data[inlineIBlockMax:]

	// i_extra_isize must be non-zero so an in-inode xattr area exists.
	extra := 32
	le.PutUint16(raw[inodeOffExtraIsize:], uint16(extra))

	hdr := 128 + extra
	le.PutUint32(raw[hdr:], xattrInodeMagic)
	entriesStart := hdr + 4

	// Single entry "system.data" (name_index = 7, name = "data").
	name := "data"
	entOff := entriesStart
	// Value is placed at the high end of the inode buffer; e_value_offs is
	// measured from entriesStart.
	valStart := len(raw) - len(overflow)
	valueOffs := valStart - entriesStart

	raw[entOff] = byte(len(name)) // e_name_len
	raw[entOff+1] = xattrIndexSystem
	le.PutUint16(raw[entOff+2:], uint16(valueOffs)) // e_value_offs
	le.PutUint32(raw[entOff+4:], 0)                 // e_value_inum
	le.PutUint32(raw[entOff+8:], uint32(len(overflow)))
	le.PutUint32(raw[entOff+12:], 0) // e_hash
	copy(raw[entOff+xattrEntrySize:], name)

	copy(raw[valStart:], overflow)
	return raw
}

// TestInlineData_IBlockResident covers a regular file whose data fits entirely
// in the 60-byte i_block area.
func TestInlineData_IBlockResident(t *testing.T) {
	data := []byte("hello inline world")
	raw := buildInlineInode(t, 0x8000, data)
	in := &inode{raw: raw, num: 12, size: uint64(len(data)), mode: 0x8000}
	sb := &superblock{BlockSize: 1024, InodeSize: 256, InodesPerGroup: 0}

	got, err := readFileData(&memFile{buf: make([]byte, 64*1024)}, 0, sb, in)
	if err != nil {
		t.Fatalf("readFileData: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("inline i_block read = %q, want %q", got, data)
	}
}

// TestInlineData_Exactly60 checks the boundary where the data exactly fills
// i_block (no xattr overflow).
func TestInlineData_Exactly60(t *testing.T) {
	data := bytes.Repeat([]byte("x"), inlineIBlockMax)
	raw := buildInlineInode(t, 0x8000, data)
	in := &inode{raw: raw, num: 5, size: uint64(len(data)), mode: 0x8000}
	sb := &superblock{BlockSize: 1024, InodeSize: 256, InodesPerGroup: 0}

	got, err := readFileData(&memFile{buf: make([]byte, 64*1024)}, 0, sb, in)
	if err != nil {
		t.Fatalf("readFileData: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("60-byte inline read mismatch")
	}
}

// TestInlineData_XattrOverflow covers a regular file larger than 60 bytes, so
// the tail of its content lives in the in-inode system.data xattr.
func TestInlineData_XattrOverflow(t *testing.T) {
	// 100 bytes: 60 in i_block, 40 in system.data.
	data := make([]byte, 100)
	for i := range data {
		data[i] = byte('A' + i%26)
	}
	raw := buildInlineInode(t, 0x8000, data)
	in := &inode{raw: raw, num: 7, size: uint64(len(data)), mode: 0x8000}
	sb := &superblock{BlockSize: 1024, InodeSize: 256, InodesPerGroup: 0}

	got, err := readFileData(&memFile{buf: make([]byte, 64*1024)}, 0, sb, in)
	if err != nil {
		t.Fatalf("readFileData: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("inline overflow read mismatch:\n got=%q\nwant=%q", got, data)
	}
}

// TestInlineDir_Entries builds an inline directory (i_block-resident) using the
// REAL kernel inline-directory layout and verifies readDir synthesises "." (the
// inode's own number) and ".." (the parent at i_block[0:4]) and parses the
// named "f1", "f2" entries from the stream at offset 4.
func TestInlineDir_Entries(t *testing.T) {
	le := binary.LittleEndian

	// Real kernel inline-directory layout:
	//   bytes[0:4]   = inode number of the PARENT ("..")
	//   bytes[4:]    = named-entry dirent stream (NO "." or ".." records)
	// The "." entry is the inode's own number and is never stored.
	const selfIno = 11
	const parentIno = 2

	// Assemble the named-entry dirent stream that starts at offset 4 of i_block.
	stream := make([]byte, inlineIBlockMax-4)
	off := 0
	writeEnt := func(ino uint32, name string, ft uint8, recLen int) {
		le.PutUint32(stream[off:], ino)
		le.PutUint16(stream[off+4:], uint16(recLen))
		stream[off+6] = byte(len(name))
		stream[off+7] = ft
		copy(stream[off+8:], name)
		off += recLen
	}
	// "f1" regular file.
	writeEnt(101, "f1", FtRegFile, minDirentSize(2))
	// "f2" regular file consumes the remaining slack so it terminates cleanly.
	writeEnt(102, "f2", FtRegFile, len(stream)-off)

	data := make([]byte, inlineIBlockMax)
	// First 4 bytes hold the parent inode number.
	le.PutUint32(data[0:], parentIno)
	copy(data[4:], stream)

	raw := make([]byte, 256)
	le.PutUint16(raw[inodeOffMode:], 0x4000) // directory
	le.PutUint32(raw[inodeOffFlags:], InodeFlagInlineData)
	le.PutUint32(raw[inodeOffSizeLo:], uint32(inlineIBlockMax))
	copy(raw[inodeOffBlock:inodeOffBlock+inlineIBlockMax], data)

	in := &inode{raw: raw, num: selfIno, size: inlineIBlockMax, mode: 0x4000}
	sb := &superblock{BlockSize: 1024, InodeSize: 256, InodesPerGroup: 0}

	entries, err := readDir(&memFile{buf: make([]byte, 64*1024)}, 0, sb, in)
	if err != nil {
		t.Fatalf("readDir: %v", err)
	}

	got := map[string]uint32{}
	for _, e := range entries {
		got[e.Name] = e.Inode
	}
	want := map[string]uint32{".": selfIno, "..": parentIno, "f1": 101, "f2": 102}
	for name, ino := range want {
		if got[name] != ino {
			t.Fatalf("entry %q = inode %d, want %d (all=%v)", name, got[name], ino, got)
		}
	}
	if len(entries) != len(want) {
		t.Fatalf("got %d entries, want %d: %v", len(entries), len(want), got)
	}
}
