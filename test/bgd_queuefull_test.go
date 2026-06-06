package filesystem_ext4_test

import (
	"testing"

	ext4 "github.com/go-filesystems/ext4"
)

func TestEnqueueQueueFull_Fallback(t *testing.T) {
	fs, cleanup := ext4.NewTempFS(t)
	defer cleanup()

	rw := ext4.CloneFSImage(t, fs)
	sb := ext4.CloneSuperblockFromFS(fs)

	var g uint32 = 0
	d, err := ext4.ReadBGD(rw, 0, sb, g)
	if err != nil {
		t.Fatalf("ReadBGD: %v", err)
	}

	// Inject a worker with a full queue to force the enqueue fallback path.
	if err := ext4.CreateFullWorker(rw, 0, sb, g, 1); err != nil {
		t.Fatalf("CreateFullWorker: %v", err)
	}
	defer ext4.RemoveWorkerFor(rw, 0, sb, g)

	starts := []int64{ext4.BgdOffset(sb, g)}
	datas := [][]byte{append([]byte(nil), ext4.BgdRaw(d)...)}

	ack, _, err := ext4.EnqueueRawCommitOps(rw, 0, sb, g, starts, datas)
	if err != nil {
		t.Fatalf("EnqueueRawCommitOps: %v", err)
	}
	if aerr := <-ack; aerr != nil {
		t.Fatalf("commit ack error (fallback): %v", aerr)
	}
}
