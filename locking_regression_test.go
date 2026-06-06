//go:build test
// +build test

package filesystem_ext4

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestDeterministicBlockingFallback reproduces a blocking-fallback scenario
// deterministically by holding the group block-bitmap lock while multiple
// allocators attempt to allocate blocks. We assert that the package-recorded
// fallback counters are incremented and that allocators complete after the
// lock is released.
func TestDeterministicBlockingFallback(t *testing.T) {
	// Shorten backoff to make the fallback behavior deterministic and fast.
	oldAttempts := backoffMaxAttempts
	oldBase := backoffBase
	backoffMaxAttempts = 2
	backoffBase = 10 * time.Microsecond
	defer func() {
		backoffMaxAttempts = oldAttempts
		backoffBase = oldBase
	}()

	// Reset counters for a clean snapshot.
	fallbackCounts = sync.Map{}

	fs, cleanup := NewTempFS(t)
	defer cleanup()

	g := uint32(0)
	// Hold the block bitmap lock so allocators can acquire the BGD but
	// contend on the bitmap lock and exercise the fallback path.
	holdUnlock := lockBitmapGroup(fs.f, g, true)

	var wg sync.WaitGroup
	goroutines := 6
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			// best-effort: ignore allocation error — we only care about
			// exercising the lock/fallback path.
			_, _ = allocBlocks(fs.f, fs.partOffset, fs.sb, 1)
		}()
	}

	// Allow goroutines to reach the fallback path.
	time.Sleep(100 * time.Millisecond)

	// Check that at least one fallback was recorded for group 0.
	var total uint64
	fallbackCounts.Range(func(k, v interface{}) bool {
		ks := k.(string)
		if strings.Contains(ks, "g:0:") {
			total += uint64(atomic.LoadUint64(v.(*uint64)))
		}
		return true
	})
	if total == 0 {
		DumpBlockingFallbacks()
		t.Fatalf("expected blocking fallback counters >0 for group 0; got 0")
	}

	// Release holder so allocators can progress and finish.
	holdUnlock()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		// ok
	case <-time.After(3 * time.Second):
		t.Fatalf("alloc goroutines did not finish in time")
	}
}
