//go:build stress
// +build stress

package filesystem_ext4_test

import (
	"math/rand"
	"sync"
	"testing"
	"time"

	ext4 "github.com/go-filesystems/ext4"
)

// TestStressConcurrentAllocFree is a longer-running stress test that exercises
// concurrent bitmap updates and BGD writes. It is guarded by the 'stress'
// build tag and does not run by default.
func TestStressConcurrentAllocFree(t *testing.T) {
	size := int64(4096) * int64(32768+18+1)
	fs, cleanup := ext4.NewTempFSWithSize(t, size)
	defer cleanup()

	rw := ext4.CloneFSImage(t, fs)
	sb := ext4.CloneSuperblockFromFS(fs)
	nGroups := int(ext4.NumBlockGroups(sb))
	rand.Seed(time.Now().UnixNano())

	var wg sync.WaitGroup
	goroutines := 10
	iters := 200
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				g := uint32(rand.Intn(nGroups))
				d, err := ext4.ReadBGD(rw, 0, sb, g)
				if err != nil {
					t.Errorf("ReadBGD: %v", err)
					return
				}
				bmp, err := ext4.ReadRawBlock(rw, 0, sb, d.BlockBitmapBlock)
				if err != nil {
					t.Errorf("ReadRawBlock: %v", err)
					return
				}
				// Flip a random free bit if available.
				max := int(sb.BlocksPerGroup)
				if max > len(bmp)*8 {
					max = len(bmp) * 8
				}
				if max == 0 {
					continue
				}
				start := rand.Intn(max)
				changed := false
				for k := 0; k < max; k++ {
					i2 := (start + k) % max
					bi := i2 / 8
					bit := uint(i2 % 8)
					if bmp[bi]&(1<<bit) == 0 {
						bmp[bi] |= 1 << bit
						changed = true
						break
					}
				}
				if !changed {
					continue
				}
				if err := ext4.WriteRawBlock(rw, 0, sb, d.BlockBitmapBlock, bmp); err != nil {
					t.Errorf("WriteRawBlock: %v", err)
					return
				}
				if err := ext4.WriteBGD(rw, 0, sb, g, d); err != nil {
					t.Errorf("WriteBGD: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}
