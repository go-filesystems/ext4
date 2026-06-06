package filesystem_ext4_test

import (
	"os"
	"os/exec"
	"sync"
	"testing"

	ext4 "github.com/go-filesystems/ext4"
)

func TestGrowAddsBlockGroups(t *testing.T) {
	fs, cleanup := ext4.NewTempFS(t)
	defer cleanup()

	sbBefore := ext4.CloneSuperblockFromFS(fs)
	oldBlocks := sbBefore.BlocksCount
	blocksPerGroup := sbBefore.BlocksPerGroup
	blockSize := sbBefore.BlockSize

	t.Logf("before: BlocksCount=%d BlocksPerGroup=%d BlockSize=%d FreeBlocks=%d", oldBlocks, blocksPerGroup, blockSize, sbBefore.FreeBlocksCount)

	// Add two full groups.
	newBlocks := oldBlocks + uint64(blocksPerGroup)*2
	newSize := int64(newBlocks) * int64(blockSize)

	if err := fs.Grow(newSize); err != nil {
		t.Fatalf("Grow failed: %v", err)
	}

	// Inspect the on-disk image copy.
	mf := ext4.CloneFSImage(t, fs)
	sbAfter, err := ext4.ReadSuperblock(mf, 0)
	if err != nil {
		t.Fatalf("ReadSuperblock after Grow: %v", err)
	}

	if sbAfter.BlocksCount != newBlocks {
		t.Fatalf("BlocksCount after Grow = %d, want %d", sbAfter.BlocksCount, newBlocks)
	}
	if sbAfter.FreeBlocksCount < sbBefore.FreeBlocksCount {
		t.Fatalf("FreeBlocksCount decreased after Grow: before=%d after=%d", sbBefore.FreeBlocksCount, sbAfter.FreeBlocksCount)
	}

	// Verify the last BGD is present and readable.
	n := ext4.NumBlockGroups(sbAfter)
	if n == 0 {
		t.Fatalf("NumBlockGroups == 0 after Grow")
	}
	if _, err := ext4.ReadBGD(mf, 0, sbAfter, n-1); err != nil {
		t.Fatalf("ReadBGD for new group failed: %v", err)
	}
}

func TestRepairImageFixesBGDMismatch(t *testing.T) {
	fs, cleanup := ext4.NewTempFS(t)
	defer cleanup()

	// Create an in-memory clone and corrupt the first group's FreeBlocksCount
	mf := ext4.CloneFSImage(t, fs)
	sb := ext4.CloneSuperblockFromFS(fs)
	off := ext4.BgdOffset(sb, 0)
	buf := ext4.HookMemFileBuf(mf)
	// Overwrite the 16-bit FreeBlocksCount at descriptor offset (standard layout).
	idx := int(off) + 12
	if idx+1 >= len(buf) {
		t.Fatalf("bgd offset out of range")
	}
	buf[idx] = 0x00
	buf[idx+1] = 0x00

	// Write corrupted image to a temp file and open it.
	tmp, err := os.CreateTemp("", "ext4-corrupt-*.img")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	name := tmp.Name()
	t.Cleanup(func() { os.Remove(name) })
	if _, err := tmp.Write(buf); err != nil {
		tmp.Close()
		t.Fatalf("write corrupted image: %v", err)
	}
	tmp.Close()

	fsi, err := ext4.Open(name, -1)
	if err != nil {
		t.Fatalf("Open corrupted image: %v", err)
	}
	fs2, ok := fsi.(*ext4.Ext4FS)
	if !ok {
		t.Fatalf("Open returned non-ext4 type")
	}

	if err := fs2.CheckImage(); err == nil {
		t.Fatalf("expected CheckImage to report inconsistency on corrupted image")
	}
	if err := fs2.RepairImage(); err != nil {
		t.Fatalf("RepairImage failed: %v", err)
	}
	if err := fs2.CheckImage(); err != nil {
		t.Fatalf("CheckImage failed after repair: %v", err)
	}
	fs2.Close()
}

func TestIntegration_GrowAndE2fsck(t *testing.T) {
	// Skip if mke2fs not present (makeTestImage handles skipping), or e2fsck not available.
	e2fsckPath, err := exec.LookPath("e2fsck")
	if err != nil {
		// common macOS brew location
		for _, c := range []string{"/opt/homebrew/sbin/e2fsck", "/usr/local/sbin/e2fsck", "/sbin/e2fsck"} {
			if _, err2 := os.Stat(c); err2 == nil {
				e2fsckPath = c
				err = nil
				break
			}
		}
	}
	if err != nil || e2fsckPath == "" {
		t.Skip("e2fsck not available; skipping integration test")
		return
	}

	// Create a small ext4 image via mke2fs (skips if mke2fs missing).
	img := makeTestImage(t, 16) // 16MB
	// Open the image with our library.
	fsi, err := ext4.Open(img, -1)
	if err != nil {
		t.Fatalf("Open formatted image: %v", err)
	}
	fsImpl, ok := fsi.(*ext4.Ext4FS)
	if !ok {
		fsi.Close()
		t.Fatalf("Open returned non-ext4 implementation")
	}
	// Grow by another 16MB.
	fi, err := os.Stat(img)
	if err != nil {
		fsImpl.Close()
		t.Fatalf("stat image: %v", err)
	}
	newSize := fi.Size() + 16*1024*1024
	if err := fsImpl.Grow(newSize); err != nil {
		fsImpl.Close()
		t.Fatalf("Grow failed: %v", err)
	}
	fsImpl.Close()

	// Run e2fsck -n (no modify) to validate consistency.
	cmd := exec.Command(e2fsckPath, "-f", "-n", img)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("e2fsck reported errors after Grow: %v\n%s", err, string(out))
	}
}

func TestConcurrentBitmapUpdates(t *testing.T) {
	fs, cleanup := ext4.NewTempFS(t)
	defer cleanup()

	// Operate on an in-memory clone so the test can run without racing the
	// real file descriptor of the open FS while still exercising locks.
	mf := ext4.CloneFSImage(t, fs)
	sb := ext4.CloneSuperblockFromFS(fs)

	// Run many small alloc/free operations concurrently to stress bitmap
	// serialization primitives.
	const goroutines = 32
	const ops = 200
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < ops; i++ {
				blocks, err := ext4.AllocBlocks(mf, 0, sb, 1)
				if err != nil {
					// Allocation can fail under pressure; that's acceptable.
					continue
				}
				for _, b := range blocks {
					// Free immediately.
					_ = ext4.FreeBlock(mf, 0, sb, b)
				}
			}
		}()
	}
	wg.Wait()

	// Validate per-group free counts vs bitmap.
	n := ext4.NumBlockGroups(sb)
	for g := uint32(0); g < n; g++ {
		d, err := ext4.ReadBGD(mf, 0, sb, g)
		if err != nil {
			t.Fatalf("ReadBGD g=%d: %v", g, err)
		}
		bmap, err := ext4.ReadBitmap(mf, 0, sb, d.BlockBitmapBlock)
		if err != nil {
			t.Fatalf("ReadBitmap g=%d: %v", g, err)
		}
		free := 0
		max := int(sb.BlocksPerGroup)
		for i := 0; i < max; i++ {
			if bmap[i/8]&(1<<uint(i%8)) == 0 {
				free++
			}
		}
		if uint32(free) != d.FreeBlocksCount {
			t.Fatalf("bg %d: free blocks mismatch after concurrent ops: bitmap=%d bgd=%d", g, free, d.FreeBlocksCount)
		}
	}
}
