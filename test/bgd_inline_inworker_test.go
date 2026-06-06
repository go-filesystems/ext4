package filesystem_ext4_test

import (
	"sync/atomic"
	"testing"

	ext4 "github.com/go-filesystems/ext4"
)

// TestInlineInWorkerFallback ensures that enqueuing a commit from inside the
// worker goroutine uses the inline-inworker fallback path instead of
// deadlocking the dispatcher.
func TestInlineInWorkerFallback(t *testing.T) {
	fs, cleanup := ext4.NewTempFS(t)
	defer cleanup()

	rw := ext4.CloneFSImage(t, fs)
	sb := ext4.CloneSuperblockFromFS(fs)

	var g uint32 = 0
	d, err := ext4.ReadBGD(rw, 0, sb, g)
	if err != nil {
		t.Fatalf("ReadBGD: %v", err)
	}

	var invoked int32
	var innerAck chan error
	errCh := make(chan error, 1)

	// Install a write hook which will be executed in the worker goroutine
	// when the worker performs the actual WriteAt. The hook will enqueue a
	// nested commit once to exercise the inline-inworker fallback path.
	rw.WriteHook = func(off int64, p []byte) error {
		if atomic.CompareAndSwapInt32(&invoked, 0, 1) {
			starts := []int64{ext4.BgdOffset(sb, g)}
			datas := [][]byte{append([]byte(nil), ext4.BgdRaw(d)...)}
			ack, _, err := ext4.EnqueueRawCommitOps(rw, 0, sb, g, starts, datas)
			if err != nil {
				select {
				case errCh <- err:
				default:
				}
				return nil
			}
			innerAck = ack
		}
		return nil
	}

	starts := []int64{ext4.BgdOffset(sb, g)}
	datas := [][]byte{append([]byte(nil), ext4.BgdRaw(d)...)}
	ack, _, err := ext4.EnqueueRawCommitOps(rw, 0, sb, g, starts, datas)
	if err != nil {
		t.Fatalf("EnqueueRawCommitOps: %v", err)
	}

	if aerr := <-ack; aerr != nil {
		t.Fatalf("outer commit error: %v", aerr)
	}

	select {
	case e := <-errCh:
		t.Fatalf("hook EnqueueRawCommitOps error: %v", e)
	default:
	}

	if innerAck == nil {
		t.Fatalf("expected inner ack to be set by hook")
	}
	if ie := <-innerAck; ie != nil {
		t.Fatalf("inner commit error: %v", ie)
	}
}
