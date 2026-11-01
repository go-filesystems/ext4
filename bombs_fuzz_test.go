package filesystem_ext4

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// fuzzAllocBudget is what one fuzz iteration may allocate: a fixed allowance
// plus a small multiple of the input. A bomb is an iteration whose allocation
// has no relation to the input's size.
func fuzzAllocBudget(inputLen int) uint64 { return 16<<20 + 64*uint64(inputLen) }

// FuzzBlockMapExtents walks a classic ext2/ext3 indirect block map built from
// arbitrary bytes: data[0:60] is i_block, data[60:68] i_size, data[68] picks
// the block size, and the whole input is the device the pointers resolve into.
func FuzzBlockMapExtents(f *testing.F) {
	seed := make([]byte, 68+4*4096)
	binary.LittleEndian.PutUint32(seed[0:], 1)
	binary.LittleEndian.PutUint32(seed[12*4:], 1)
	binary.LittleEndian.PutUint64(seed[60:], 5000)
	f.Add(seed)
	f.Add(make([]byte, 69))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 69 {
			return
		}
		bs := uint32(1024) << (data[68] % 3)
		in := bombInode(0, binary.LittleEndian.Uint64(data[60:]))
		copy(in.raw[inodeOffBlock:inodeOffBlock+60], data[:60])
		img := &bombFile{buf: data[69:]}
		for _, declared := range []uint64{0, 1 << 40} {
			sb := &superblock{BlockSize: bs, InodeSize: 256, BlocksCount: declared, deviceBytes: int64(len(img.buf)), maxRead: 1 << 20}
			got := allocDelta(func() {
				leaves, err := in.blockMapExtents(img, 0, sb)
				if err == nil {
					_ = validateExtents(leaves, 2)
				}
				if _, err := in.blockMapData(img, 0, sb); err == nil && uint64(in.size) > uint64(sb.fileSizeCeiling(0)) {
					t.Fatalf("size %d accepted over the ceiling", in.size)
				}
			})
			if got > fuzzAllocBudget(len(data)) {
				t.Fatalf("allocated %d bytes for a %d-byte image", got, len(data))
			}
		}
	})
}

// FuzzExtentList feeds an arbitrary extent list through validateExtents and the
// directory reader.
func FuzzExtentList(f *testing.F) {
	f.Add(make([]byte, 60+8192))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 60 {
			return
		}
		in := bombInode(InodeFlagExtents|0, 1<<20)
		in.mode = 0x4000
		copy(in.raw[inodeOffBlock:inodeOffBlock+60], data[:60])
		img := &bombFile{buf: data[60:]}
		sb := &superblock{BlockSize: 1024, InodeSize: 256, BlocksCount: uint64(len(img.buf)/1024) + 1, deviceBytes: int64(len(img.buf))}
		got := allocDelta(func() { _, _ = readDir(img, 0, sb, in) })
		if got > fuzzAllocBudget(len(data)) {
			t.Fatalf("readDir allocated %d bytes for a %d-byte image", got, len(data))
		}
	})
}

// FuzzJournalSidecar replays and scans an arbitrary sidecar journal file.
func FuzzJournalSidecar(f *testing.F) {
	le := binary.LittleEndian
	var seed []byte
	seed = le.AppendUint32(seed, journalMagic)
	seed = le.AppendUint64(seed, 1)
	seed = le.AppendUint32(seed, 1)
	seed = le.AppendUint64(seed, 1)
	seed = le.AppendUint32(seed, 4)
	seed = append(seed, 1, 2, 3, 4)
	f.Add(seed)
	f.Add(seed[:16])
	f.Fuzz(func(t *testing.T, data []byte) {
		p := filepath.Join(t.TempDir(), "img.journal")
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		for _, replay := range []bool{false, true} {
			js, err := os.OpenFile(p, os.O_RDWR, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			j := &Journal{js: js, enabled: true, f: &memFile{buf: make([]byte, 16*1024)}, blockSize: 1024, sb: &superblock{BlocksCount: 16, BlockSize: 1024}}
			got := allocDelta(func() {
				if replay {
					_ = j.ReplayOnOpen()
				} else {
					_ = j.initNextSeq()
				}
			})
			js.Close()
			if got > fuzzAllocBudget(len(data)) {
				t.Fatalf("allocated %d bytes for a %d-byte journal (replay=%v)", got, len(data), replay)
			}
		}
	})
}
