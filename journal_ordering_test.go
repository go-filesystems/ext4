package filesystem_ext4

import (
	"bytes"
	"testing"
)

// TestJournalCommitOrdering verifies that journal-applied writes participate
// in the same per-file+group commit ordering as non-journal enqueues so that
// writes do not interleave and the final state is deterministic.
func TestJournalCommitOrdering(t *testing.T) {
	fs, cleanup := NewTempFS(t)
	defer cleanup()

	rw := getRW(fs)
	j := journalForAny(rw)
	if j == nil || !j.enabled {
		t.Fatalf("expected journal enabled for temp fs")
	}

	sb := fs.sb
	// choose a block group that exists (prefer group 1 when available)
	g := uint32(1)
	if sb.numBlockGroups() <= 1 {
		g = 0
	}
	groupStart := uint64(g)*uint64(sb.BlocksPerGroup) + uint64(sb.FirstDataBlock)
	targetBlock := groupStart + 3

	bs := int(sb.BlockSize)
	data1 := bytes.Repeat([]byte{0x11}, bs)
	data2 := bytes.Repeat([]byte{0x22}, bs)

	// Enqueue a non-journal commit (no explicit BGD lock held here) and
	// then perform a journaled transaction that updates the same block.
	// Both paths should serialize via the commit dispatcher and the final
	// result should reflect the last-committed write.
	off := fs.partOffset + int64(targetBlock)*int64(sb.BlockSize)
	ops := []commitOp{{startAbs: off, data: data1}}
	if err := enqueueCommitWrites(rw, fs.partOffset, sb, g, ops); err != nil {
		t.Fatalf("non-journal enqueue failed: %v", err)
	}

	var tx *Transaction
	var err error
	tx, err = j.StartTx()
	if err != nil {
		t.Fatalf("StartTx: %v", err)
	}
	if err := addRangeToTx(tx, rw, fs.partOffset, sb, off, data2); err != nil {
		t.Fatalf("addRangeToTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Verify the final block content matches the last-committed data (data2).
	// reuse `off` from above
	buf := make([]byte, bs)
	if _, err := rw.ReadAt(buf, off); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(buf, data2) {
		t.Fatalf("unexpected final block content; want data2, got first byte 0x%02x", buf[0])
	}
}
