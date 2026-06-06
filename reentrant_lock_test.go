package filesystem_ext4

import (
	"testing"
	"time"
)

func TestReentrantMutexOwner(t *testing.T) {
	var r reentrantMutex
	owner := NewOwner()
	r.LockOwner(owner)
	r.LockOwner(owner)
	r.UnlockOwner(owner)
	r.UnlockOwner(owner)
	if r.Owner() != 0 {
		t.Fatalf("owner not cleared: %d", r.Owner())
	}
}

func TestReentrantMutexExclusive(t *testing.T) {
	var r reentrantMutex
	owner1 := NewOwner()
	owner2 := NewOwner()
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		r.LockOwner(owner1)
		close(started)
		// hold lock a short while
		time.Sleep(50 * time.Millisecond)
		r.UnlockOwner(owner1)
		close(done)
	}()
	// wait for goroutine to acquire the lock
	<-started
	start := time.Now()
	r.LockOwner(owner2)
	elapsed := time.Since(start)
	r.UnlockOwner(owner2)
	if elapsed < 40*time.Millisecond {
		t.Fatalf("expected LockOwner to block until owner1 released; elapsed=%v", elapsed)
	}
}
