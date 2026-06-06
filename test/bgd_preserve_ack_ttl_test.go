package filesystem_ext4_test

import (
	"testing"
	"time"

	ext4 "github.com/go-filesystems/ext4"
)

// TestBGDPreservesOtherFields verifies that WriteBGD updates free counts but
// preserves other descriptor fields (e.g., InodeTableBlock and Flags).
func TestBGDPreservesOtherFields(t *testing.T) {
	fs, cleanup := ext4.NewTempFS(t)
	defer cleanup()

	rw := ext4.CloneFSImage(t, fs)
	sb := ext4.CloneSuperblockFromFS(fs)

	var g uint32 = 0

	d, err := ext4.ReadBGD(rw, 0, sb, g)
	if err != nil {
		t.Fatalf("ReadBGD: %v", err)
	}
	// Prepare a modified raw descriptor that changes non-count fields.
	origRaw := ext4.BgdRaw(d)
	raw := make([]byte, len(origRaw))
	copy(raw, origRaw)
	// Tweak the inode-table field (offset 8..11) and flags (offset 18..19)
	raw[8] = 0x12
	raw[9] = 0x34
	raw[10] = 0x56
	raw[11] = 0x78
	raw[18] = 0xBE
	raw[19] = 0xEF

	// Decode a descriptor object backed by our modified raw bytes so the
	// worker will write our modified bytes (with counts recomputed).
	dMod := ext4.DecodeBGD(raw, sb)

	// Mutate block bitmap so FreeBlocksCount will change and trigger a recompute.
	bmp, err := ext4.ReadRawBlock(rw, 0, sb, d.BlockBitmapBlock)
	if err != nil {
		t.Fatalf("ReadRawBlock: %v", err)
	}
	changed := false
	for i := 0; i < len(bmp)*8; i++ {
		if i >= int(sb.BlocksPerGroup) {
			break
		}
		bi := i / 8
		bit := uint(i % 8)
		if bmp[bi]&(1<<bit) == 0 {
			bmp[bi] |= 1 << bit
			changed = true
			break
		}
	}
	if !changed {
		t.Fatalf("no free bit found to flip in bitmap")
	}
	if err := ext4.WriteRawBlock(rw, 0, sb, d.BlockBitmapBlock, bmp); err != nil {
		t.Fatalf("WriteRawBlock: %v", err)
	}

	newFree := func() int {
		max := int(sb.BlocksPerGroup)
		if max > len(bmp)*8 {
			max = len(bmp) * 8
		}
		cnt := 0
		for i := 0; i < max; i++ {
			if bmp[i/8]&(1<<uint(i%8)) == 0 {
				cnt++
			}
		}
		return cnt
	}()

	if err := ext4.WriteBGD(rw, 0, sb, g, dMod); err != nil {
		t.Fatalf("WriteBGD: %v", err)
	}

	d2, err := ext4.ReadBGD(rw, 0, sb, g)
	if err != nil {
		t.Fatalf("ReadBGD after write: %v", err)
	}

	// Verify counts were updated
	if int(d2.FreeBlocksCount) != newFree {
		t.Fatalf("BGD FreeBlocksCount mismatch: expected=%d got=%d", newFree, d2.FreeBlocksCount)
	}

	// Verify our non-count fields were preserved in the descriptor bytes.
	if d2.InodeTableBlock&0xFFFFFFFF != 0x78563412 {
		t.Fatalf("InodeTableBlock not preserved: got=0x%x", d2.InodeTableBlock&0xFFFFFFFF)
	}
	if d2.Flags != 0xEFBE { // little-endian bytes BE EF -> 0xEFBE
		t.Fatalf("Flags not preserved: got=0x%x", d2.Flags)
	}
}

// TestAckToSeqTTL verifies that the ack->seq mapping is retained for the
// configured grace period and removed after the TTL elapses.
func TestAckToSeqTTL(t *testing.T) {
	fs, cleanup := ext4.NewTempFS(t)
	defer cleanup()

	rw := ext4.CloneFSImage(t, fs)
	sb := ext4.CloneSuperblockFromFS(fs)

	var g uint32 = 0

	d, err := ext4.ReadBGD(rw, 0, sb, g)
	if err != nil {
		t.Fatalf("ReadBGD: %v", err)
	}

	// Set a TTL of 200ms for diagnostics.
	ext4.SetAckToSeqDeleteGraceMS(200)

	starts := []int64{ext4.BgdOffset(sb, g)}
	datas := [][]byte{append([]byte(nil), ext4.BgdRaw(d)...)}

	ack, seq, err := ext4.EnqueueRawCommitOps(rw, 0, sb, g, starts, datas)
	if err != nil {
		t.Fatalf("EnqueueRawCommitOps: %v", err)
	}

	// Mapping should exist immediately after enqueue.
	if s, ok := ext4.GetSeqForAck(ack); !ok || s != seq {
		t.Fatalf("ack->seq mapping missing immediately after enqueue: ok=%v seq=%d got=%d", ok, seq, s)
	}

	// Short wait: still present
	time.Sleep(50 * time.Millisecond)
	if _, ok := ext4.GetSeqForAck(ack); !ok {
		t.Fatalf("ack->seq mapping unexpectedly deleted early")
	}

	// Wait for TTL + small buffer then expect deletion.
	time.Sleep(250 * time.Millisecond)
	if _, ok := ext4.GetSeqForAck(ack); ok {
		t.Fatalf("ack->seq mapping not deleted after TTL")
	}

	// Drain ack to observe any worker error.
	if aerr := <-ack; aerr != nil {
		t.Fatalf("commit returned error: %v", aerr)
	}
}
