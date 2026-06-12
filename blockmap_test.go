package filesystem_ext4

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// TestBlockMapData_DirectIndirectHoles exercises the classic ext2/ext3
// indirect block-map read path with no external tools: a hand-built image
// covering direct blocks, sparse holes (pointer 0), a single-indirect block,
// and a partial trailing block.
func TestBlockMapData_DirectIndirectHoles(t *testing.T) {
	const bs = 1024
	le := binary.LittleEndian
	backing := make([]byte, 64*bs)
	put := func(block int, b byte, n int) {
		for i := 0; i < n; i++ {
			backing[block*bs+i] = b
		}
	}
	// Data blocks.
	put(10, 'A', bs) // logical 0
	put(11, 'C', bs) // logical 2
	put(30, 'M', bs) // logical 12 (reached via single-indirect)
	// Single-indirect block at physical block 20: entry[0] -> block 30.
	le.PutUint32(backing[20*bs+0:], 30)

	// Inode: 256 bytes, regular file, block-map (flags = 0).
	raw := make([]byte, 256)
	le.PutUint16(raw[inodeOffMode:], 0x8000)
	// i_block: direct[0]=10, [1]=0 hole, [2]=11, [3..11]=0, [12]=20 (s-indirect).
	le.PutUint32(raw[inodeOffBlock+0*4:], 10)
	le.PutUint32(raw[inodeOffBlock+2*4:], 11)
	le.PutUint32(raw[inodeOffBlock+12*4:], 20)

	// size: logical blocks 0..11 plus 500 bytes of logical block 12.
	size := 12*bs + 500
	in := &inode{raw: raw, num: 2, size: uint64(size), mode: 0x8000}

	// InodesPerGroup = 0 makes readFileData skip the on-disk inode re-read,
	// so our synthetic inode is used as-is.
	sb := &superblock{BlockSize: bs, InodeSize: 256, InodesPerGroup: 0}

	got, err := readFileData(&memFile{buf: backing}, 0, sb, in)
	if err != nil {
		t.Fatalf("readFileData: %v", err)
	}
	if len(got) != size {
		t.Fatalf("len = %d, want %d", len(got), size)
	}

	want := make([]byte, size)
	for i := 0; i < bs; i++ {
		want[0*bs+i] = 'A' // logical 0
	}
	for i := 0; i < bs; i++ {
		want[2*bs+i] = 'C' // logical 2
	}
	// logical 1 and 3..11 are holes -> zero (already).
	for i := 0; i < 500; i++ {
		want[12*bs+i] = 'M' // logical 12, partial
	}
	if !bytes.Equal(got, want) {
		// Point at the first divergence to keep failures readable.
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("byte %d: got %q want %q", i, got[i], want[i])
			}
		}
		t.Fatalf("content mismatch (len ok)")
	}
}
