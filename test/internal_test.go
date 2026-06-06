package filesystem_ext4_test

import (
	"encoding/binary"
	"testing"

	ext4 "github.com/go-filesystems/ext4"
)

// ---------------------------------------------------------------------------
// crc32c
// ---------------------------------------------------------------------------

func TestCRC32c_EmptyData(t *testing.T) {
	if got := ext4.CRC32c(0, nil); got != 0 {
		t.Errorf("CRC32c(0, nil) = 0x%08X, want 0", got)
	}
}

func TestCRC32c_KnownVector(t *testing.T) {
	got := ext4.CRC32c(0, []byte("123456789"))
	const want = uint32(0xE3069283)
	if got != want {
		t.Errorf("CRC32c of 123456789 = 0x%08X, want 0x%08X", got, want)
	}
}

func TestCRC32c_Incremental(t *testing.T) {
	data := []byte("The quick brown fox jumps over the lazy dog")
	const seed = uint32(0xDEADBEEF)
	oneShot := ext4.CRC32c(seed, data)
	split := ext4.CRC32c(ext4.CRC32c(seed, data[:20]), data[20:])
	if oneShot != split {
		t.Errorf("incremental CRC32c mismatch: one-shot=0x%08X split=0x%08X", oneShot, split)
	}
}

func TestCRC32c_SeedPropagates(t *testing.T) {
	a := ext4.CRC32c(0, []byte("hello"))
	b := ext4.CRC32c(0xFFFFFFFF, []byte("hello"))
	if a == b {
		t.Errorf("different seeds should produce different checksums")
	}
}

// ---------------------------------------------------------------------------
// setBit / clearBit
// ---------------------------------------------------------------------------

func TestSetBit(t *testing.T) {
	bmap := make([]byte, 4)
	for i := 0; i < 32; i++ {
		ext4.SetBit(bmap, i)
		if bmap[i/8]&(1<<uint(i%8)) == 0 {
			t.Errorf("SetBit(%d): bit not set", i)
		}
	}
}

func TestClearBit(t *testing.T) {
	bmap := []byte{0xFF, 0xFF, 0xFF, 0xFF}
	for i := 0; i < 32; i++ {
		ext4.ClearBit(bmap, i)
		if bmap[i/8]&(1<<uint(i%8)) != 0 {
			t.Errorf("ClearBit(%d): bit not cleared", i)
		}
	}
}

func TestSetClearBit_Roundtrip(t *testing.T) {
	bmap := make([]byte, 2)
	for i := 0; i < 16; i++ {
		ext4.SetBit(bmap, i)
		ext4.ClearBit(bmap, i)
		if bmap[i/8]&(1<<uint(i%8)) != 0 {
			t.Errorf("set+clear roundtrip at bit %d left bit set", i)
		}
	}
}

// ---------------------------------------------------------------------------
// findFreeBit
// ---------------------------------------------------------------------------

func TestFindFreeBit_AllFree(t *testing.T) {
	bmap := make([]byte, 8)
	bit, ok := ext4.FindFreeBit(bmap, 64)
	if !ok || bit != 0 {
		t.Errorf("all-zero bitmap: got (%d, %v), want (0, true)", bit, ok)
	}
}

func TestFindFreeBit_FirstSet(t *testing.T) {
	bmap := make([]byte, 8)
	ext4.SetBit(bmap, 0)
	bit, ok := ext4.FindFreeBit(bmap, 64)
	if !ok || bit != 1 {
		t.Errorf("first bit set: got (%d, %v), want (1, true)", bit, ok)
	}
}

func TestFindFreeBit_AllSet(t *testing.T) {
	bmap := []byte{0xFF, 0xFF, 0xFF, 0xFF}
	_, ok := ext4.FindFreeBit(bmap, 32)
	if ok {
		t.Errorf("all-set bitmap: expected not found")
	}
}

func TestFindFreeBit_MiddleFree(t *testing.T) {
	bmap := make([]byte, 4)
	for i := 0; i < 17; i++ {
		ext4.SetBit(bmap, i)
	}
	bit, ok := ext4.FindFreeBit(bmap, 32)
	if !ok || bit != 17 {
		t.Errorf("first 17 bits set: got (%d, %v), want (17, true)", bit, ok)
	}
}

func TestFindFreeBit_LastBit(t *testing.T) {
	bmap := make([]byte, 4)
	for i := 0; i < 31; i++ {
		ext4.SetBit(bmap, i)
	}
	bit, ok := ext4.FindFreeBit(bmap, 32)
	if !ok || bit != 31 {
		t.Errorf("last bit free: got (%d, %v), want (31, true)", bit, ok)
	}
}

func TestFindFreeBit_BeyondMaxBits(t *testing.T) {
	bmap := []byte{0xFF, 0x00}
	_, ok := ext4.FindFreeBit(bmap, 8)
	if ok {
		t.Errorf("bit beyond maxBits should not be found")
	}
}

// ---------------------------------------------------------------------------
// findFreeRun
// ---------------------------------------------------------------------------

func TestFindFreeRun_FullRunContiguous(t *testing.T) {
	bmap := make([]byte, 16)
	bits, ok := ext4.FindFreeRun(bmap, 128, 5)
	if !ok {
		t.Fatal("expected to find a free run of 5 in empty bitmap")
	}
	if len(bits) != 5 {
		t.Fatalf("expected 5 bits, got %d", len(bits))
	}
	for i, b := range bits {
		if b != i {
			t.Errorf("bits[%d] = %d, want %d", i, b, i)
		}
	}
}

func TestFindFreeRun_AvoidsSetBits(t *testing.T) {
	bmap := make([]byte, 16)
	ext4.SetBit(bmap, 0)
	ext4.SetBit(bmap, 1)
	ext4.SetBit(bmap, 2)
	bits, ok := ext4.FindFreeRun(bmap, 128, 3)
	if !ok {
		t.Fatal("expected to find run")
	}
	if bits[0] != 3 {
		t.Errorf("run should start at bit 3, got %d", bits[0])
	}
}

func TestFindFreeRun_FallbackNonContiguous(t *testing.T) {
	bmap := make([]byte, 16)
	for i := 0; i < 128; i += 2 {
		ext4.SetBit(bmap, i)
	}
	bits, ok := ext4.FindFreeRun(bmap, 128, 3)
	if !ok {
		t.Fatal("non-contiguous fallback should find 3 free bits")
	}
	if len(bits) != 3 {
		t.Fatalf("expected 3 bits, got %d", len(bits))
	}
	for _, b := range bits {
		if b%2 == 0 {
			t.Errorf("bit %d is set but was returned as free", b)
		}
	}
}

func TestFindFreeRun_NotEnoughBits(t *testing.T) {
	bmap := []byte{0b11111100}
	_, ok := ext4.FindFreeRun(bmap, 8, 3)
	if ok {
		t.Error("should not find 3 free bits when only 2 exist")
	}
}

func TestFindFreeRun_ZeroBlocks(t *testing.T) {
	bmap := make([]byte, 8)
	bits, ok := ext4.FindFreeRun(bmap, 64, 0)
	if !ok {
		t.Error("zero-length run should always succeed")
	}
	if len(bits) != 0 {
		t.Errorf("zero-length run should return empty slice, got %v", bits)
	}
}

func TestFindFreeRun_ExactFit(t *testing.T) {
	bmap := make([]byte, 1)
	bmap[0] = 0b00001111
	bits, ok := ext4.FindFreeRun(bmap, 8, 4)
	if !ok {
		t.Fatal("expected to find run of 4")
	}
	if bits[0] != 4 {
		t.Errorf("expected start at bit 4, got %d", bits[0])
	}
}

// ---------------------------------------------------------------------------
// buildExtents
// ---------------------------------------------------------------------------

func TestBuildExtents_Empty(t *testing.T) {
	exts := ext4.BuildExtents(nil)
	if len(exts) != 0 {
		t.Errorf("empty input: expected no extents, got %d", len(exts))
	}
}

func TestBuildExtents_Single(t *testing.T) {
	exts := ext4.BuildExtents([]uint64{100})
	if len(exts) != 1 {
		t.Fatalf("single block: expected 1 extent, got %d", len(exts))
	}
	if exts[0].PhysBlock != 100 || exts[0].Count != 1 || exts[0].LogBlock != 0 {
		t.Errorf("unexpected extent: %+v", exts[0])
	}
}

func TestBuildExtents_Contiguous(t *testing.T) {
	phys := []uint64{50, 51, 52, 53, 54}
	exts := ext4.BuildExtents(phys)
	if len(exts) != 1 {
		t.Fatalf("contiguous: expected 1 extent, got %d", len(exts))
	}
	e := exts[0]
	if e.PhysBlock != 50 || e.Count != 5 || e.LogBlock != 0 {
		t.Errorf("unexpected extent: %+v", e)
	}
}

func TestBuildExtents_TwoRuns(t *testing.T) {
	phys := []uint64{10, 11, 12, 20, 21}
	exts := ext4.BuildExtents(phys)
	if len(exts) != 2 {
		t.Fatalf("two runs: expected 2 extents, got %d", len(exts))
	}
	if exts[0].PhysBlock != 10 || exts[0].Count != 3 || exts[0].LogBlock != 0 {
		t.Errorf("first extent wrong: %+v", exts[0])
	}
	if exts[1].PhysBlock != 20 || exts[1].Count != 2 || exts[1].LogBlock != 3 {
		t.Errorf("second extent wrong: %+v", exts[1])
	}
}

func TestBuildExtents_FourSeparateBlocks(t *testing.T) {
	phys := []uint64{1, 3, 5, 7}
	exts := ext4.BuildExtents(phys)
	if len(exts) != 4 {
		t.Fatalf("all separate: expected 4 extents, got %d", len(exts))
	}
	for i, e := range exts {
		if e.Count != 1 {
			t.Errorf("extent[%d]: count = %d, want 1", i, e.Count)
		}
		if e.LogBlock != uint32(i) {
			t.Errorf("extent[%d]: LogBlock = %d, want %d", i, e.LogBlock, i)
		}
	}
}

func TestBuildExtents_LogBlockMonotonicallyIncreasing(t *testing.T) {
	phys := []uint64{100, 101, 200, 201, 202, 300}
	exts := ext4.BuildExtents(phys)
	if len(exts) != 3 {
		t.Fatalf("expected 3 extents, got %d", len(exts))
	}
	expectedLog := []uint32{0, 2, 5}
	for i, e := range exts {
		if e.LogBlock != expectedLog[i] {
			t.Errorf("extent[%d]: LogBlock = %d, want %d", i, e.LogBlock, expectedLog[i])
		}
	}
}

// ---------------------------------------------------------------------------
// minDirentSize
// ---------------------------------------------------------------------------

func TestMinDirentSize_Zero(t *testing.T) {
	if got := ext4.MinDirentSize(0); got != 8 {
		t.Errorf("MinDirentSize(0) = %d, want 8", got)
	}
}

func TestMinDirentSize_AlignedTo4(t *testing.T) {
	cases := []struct{ nameLen, want int }{{1, 12}, {2, 12}, {3, 12}, {4, 12}, {5, 16}, {8, 16}, {9, 20}, {255, 264}}
	for _, c := range cases {
		if got := ext4.MinDirentSize(c.nameLen); got != c.want {
			t.Errorf("MinDirentSize(%d) = %d, want %d", c.nameLen, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// parseDirBlock / writeDirEntry / tryInsertDirEntry
// ---------------------------------------------------------------------------

// buildDirBlock constructs a synthetic directory block with the given entries.
func buildDirBlock(blockSize int, entries []struct {
	ino      uint32
	name     string
	fileType uint8
}) []byte {
	le := binary.LittleEndian
	buf := make([]byte, blockSize)
	off := 0
	for i, e := range entries {
		min := ext4.MinDirentSize(len(e.name))
		recLen := min
		if i == len(entries)-1 {
			recLen = blockSize - off
		}
		le.PutUint32(buf[off:], e.ino)
		le.PutUint16(buf[off+4:], uint16(recLen))
		buf[off+6] = uint8(len(e.name))
		buf[off+7] = e.fileType
		copy(buf[off+8:], e.name)
		off += recLen
	}
	return buf
}

func TestParseDirBlock_Empty(t *testing.T) {
	buf := make([]byte, 4096)
	entries := ext4.ParseDirBlock(buf)
	if len(entries) != 0 {
		t.Errorf("zeroed block: expected 0 entries, got %d", len(entries))
	}
}

func TestParseDirBlock_SingleEntry(t *testing.T) {
	entries := []struct {
		ino      uint32
		name     string
		fileType uint8
	}{{2, ".", ext4.FtDir}}
	buf := buildDirBlock(4096, entries)
	got := ext4.ParseDirBlock(buf)
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(got))
	}
	if got[0].Inode != 2 || got[0].Name != "." || got[0].FileType != ext4.FtDir {
		t.Errorf("unexpected entry: %+v", got[0])
	}
}

func TestParseDirBlock_MultipleEntries(t *testing.T) {
	raw := []struct {
		ino      uint32
		name     string
		fileType uint8
	}{{2, ".", ext4.FtDir}, {2, "..", ext4.FtDir}, {10, "etc", ext4.FtDir}, {11, "usr", ext4.FtDir}, {12, "var", ext4.FtDir}}
	buf := buildDirBlock(4096, raw)
	got := ext4.ParseDirBlock(buf)
	if len(got) != len(raw) {
		t.Fatalf("expected %d entries, got %d", len(raw), len(got))
	}
	for i, e := range raw {
		if got[i].Name != e.name || got[i].Inode != e.ino || got[i].FileType != e.fileType {
			t.Errorf("entry[%d]: got %+v, want %+v", i, got[i], e)
		}
	}
}

func TestParseDirBlock_SkipsZeroInode(t *testing.T) {
	le := binary.LittleEndian
	buf := make([]byte, 4096)
	le.PutUint32(buf[0:], 0)
	le.PutUint16(buf[4:], 12)
	buf[6] = 3
	buf[7] = ext4.FtRegFile
	copy(buf[8:], "foo")
	le.PutUint32(buf[12:], 5)
	le.PutUint16(buf[16:], 4096-12)
	buf[18] = 3
	buf[19] = ext4.FtRegFile
	copy(buf[20:], "bar")
	got := ext4.ParseDirBlock(buf)
	if len(got) != 1 {
		t.Fatalf("expected 1 entry (inode-0 skipped), got %d", len(got))
	}
	if got[0].Name != "bar" || got[0].Inode != 5 {
		t.Errorf("unexpected entry: %+v", got[0])
	}
}

func TestParseDirBlock_SkipsFtDirTail(t *testing.T) {
	le := binary.LittleEndian
	buf := make([]byte, 4096)
	le.PutUint32(buf[0:], 7)
	le.PutUint16(buf[4:], 12)
	buf[6] = 3
	buf[7] = ext4.FtDir
	copy(buf[8:], "lib")
	tailOff := 4084
	le.PutUint16(buf[tailOff+4:], 12)
	buf[tailOff+7] = ext4.FtDirTail
	got := ext4.ParseDirBlock(buf)
	for _, e := range got {
		if e.FileType == ext4.FtDirTail {
			t.Errorf("ParseDirBlock returned FtDirTail entry: %+v", e)
		}
	}
}

// ---------------------------------------------------------------------------
// writeDirEntry / tryInsertDirEntry
// ---------------------------------------------------------------------------

func TestWriteDirEntry_FieldsCorrect(t *testing.T) {
	le := binary.LittleEndian
	buf := make([]byte, 64)
	ext4.WriteDirEntry(buf, 0, 42, 20, "hello", ext4.FtRegFile)
	if ino := le.Uint32(buf[0:]); ino != 42 {
		t.Errorf("inode: got %d, want 42", ino)
	}
	if rl := le.Uint16(buf[4:]); rl != 20 {
		t.Errorf("rec_len: got %d, want 20", rl)
	}
	if nl := buf[6]; nl != 5 {
		t.Errorf("name_len: got %d, want 5", nl)
	}
	if ft := buf[7]; ft != ext4.FtRegFile {
		t.Errorf("file_type: got %d, want %d", ft, ext4.FtRegFile)
	}
	if name := string(buf[8:13]); name != "hello" {
		t.Errorf("name: got %q, want %q", name, "hello")
	}
}

func TestWriteDirEntry_OffsetRespected(t *testing.T) {
	le := binary.LittleEndian
	buf := make([]byte, 64)
	buf[0] = 0xAA
	ext4.WriteDirEntry(buf, 16, 99, 12, "go", ext4.FtRegFile)
	if buf[0] != 0xAA {
		t.Error("WriteDirEntry wrote before its offset")
	}
	if ino := le.Uint32(buf[16:]); ino != 99 {
		t.Errorf("inode at offset 16: got %d, want 99", ino)
	}
}

func TestTryInsertDirEntry_HasSlack(t *testing.T) {
	le := binary.LittleEndian
	buf := make([]byte, 4096)
	le.PutUint32(buf[0:], 10)
	le.PutUint16(buf[4:], 4096)
	buf[6] = 3
	buf[7] = ext4.FtDir
	copy(buf[8:], "foo")
	ok := ext4.TryInsertDirEntry(buf, 20, "bar", ext4.FtRegFile, ext4.MinDirentSize(3))
	if !ok {
		t.Fatal("expected insertion to succeed")
	}
	if rl := le.Uint16(buf[4:]); rl != 12 {
		t.Errorf("foo rec_len after split: got %d, want 12", rl)
	}
	if ino := le.Uint32(buf[12:]); ino != 20 {
		t.Errorf("bar inode: got %d, want 20", ino)
	}
	if name := string(buf[20:23]); name != "bar" {
		t.Errorf("bar name: got %q, want %q", name, "bar")
	}
}

func TestTryInsertDirEntry_NoSlack(t *testing.T) {
	le := binary.LittleEndian
	buf := make([]byte, 4096)
	le.PutUint32(buf[0:], 10)
	le.PutUint16(buf[4:], 12)
	buf[6] = 3
	buf[7] = ext4.FtDir
	copy(buf[8:], "foo")
	ok := ext4.TryInsertDirEntry(buf, 20, "bar", ext4.FtRegFile, ext4.MinDirentSize(3))
	if ok {
		t.Error("expected insertion to fail (no slack)")
	}
}

func TestTryInsertDirEntry_EmptyBlock(t *testing.T) {
	buf := make([]byte, 4096)
	ok := ext4.TryInsertDirEntry(buf, 20, "bar", ext4.FtRegFile, ext4.MinDirentSize(3))
	if ok {
		t.Error("expected insertion to fail on empty block")
	}
}
