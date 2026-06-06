package filesystem_ext4_test

import (
	"testing"
	"time"

	ext4 "github.com/go-filesystems/ext4"
)

func TestCommitWorkerIdle_Cleanup(t *testing.T) {
	old := ext4.GetCommitWorkerIdleSecs()
	ext4.SetCommitWorkerIdleSecs(1)
	defer ext4.SetCommitWorkerIdleSecs(old)

	fs, cleanup := ext4.NewTempFS(t)
	defer cleanup()

	rw := ext4.CloneFSImage(t, fs)
	sb := ext4.CloneSuperblockFromFS(fs)
	var g uint32 = 0

	// enqueue a small commit to ensure a worker is created
	d, err := ext4.ReadBGD(rw, 0, sb, g)
	if err != nil {
		t.Fatalf("ReadBGD: %v", err)
	}
	starts := []int64{ext4.BgdOffset(sb, g)}
	datas := [][]byte{append([]byte(nil), ext4.BgdRaw(d)...)}
	ack, _, err := ext4.EnqueueRawCommitOps(rw, 0, sb, g, starts, datas)
	if err != nil {
		t.Fatalf("EnqueueRawCommitOps: %v", err)
	}
	if aerr := <-ack; aerr != nil {
		t.Fatalf("enqueue commit ack: %v", aerr)
	}

	// wait longer than the idle duration to allow cleanup
	time.Sleep(1500 * time.Millisecond)
	if ext4.WorkerExistsFor(rw, 0, sb, g) {
		t.Fatalf("commit worker still exists after idle timeout")
	}
}
