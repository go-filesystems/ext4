package filesystem_ext4_test

import (
	"testing"

	ext4 "github.com/go-filesystems/ext4"
)

func TestMarkSeqAcked_IsSeqAcked(t *testing.T) {
	seq := uint64(424242)
	if ext4.IsSeqAcked(seq) {
		t.Fatalf("unexpected seq %d already acked", seq)
	}
	ext4.MarkSeqAcked(seq)
	if !ext4.IsSeqAcked(seq) {
		t.Fatalf("seq %d was not marked acked", seq)
	}
}
