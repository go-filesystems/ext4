package filesystem_ext4_test

import (
	"testing"

	ext4 "github.com/go-filesystems/ext4"
)

func TestJournalStub_StartCommit(t *testing.T) {
	j, err := ext4.OpenJournal(nil, 0, 4096, nil)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	tx, err := j.StartTx()
	if err != nil {
		t.Fatalf("StartTx: %v", err)
	}
	if err := tx.AddBlock(123, []byte("hello")); err != nil {
		t.Fatalf("AddBlock: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// Second commit should report an error (already committed)
	if err := tx.Commit(); err == nil {
		t.Fatalf("expected error on second Commit()")
	}
}
