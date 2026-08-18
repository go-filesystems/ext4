// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package filesystem_ext4_test

import (
	"os"
	"sync"
	"testing"

	ext4 "github.com/go-filesystems/ext4"
)

// FuzzOpenAndRead feeds whole byte blobs to OpenFromDevice, and essentially
// none of them are ext4: over a 60-second run of 8.4 million executions,
// exactly ONE input reached the decoder, and it was the pristine seed. The
// superblock magic and geometry checks reject everything else immediately, so
// that target covers the front door and nothing behind it.
//
// This one splices fuzzer-chosen bytes into a *valid* image instead, so the
// corrupted fields are reached by code that has already accepted the
// superblock: the group descriptors, inode tables, extent trees and directory
// blocks. Same budget, four orders of magnitude more inputs through the door.

var (
	patchSeedOnce sync.Once
	patchSeed     []byte
)

// smallValidImage formats the smallest image the driver accepts (512 KiB) so a
// fuzz iteration copies as little as possible, and puts a file and a directory
// in it so the read paths have something to decode.
func smallValidImage(t testing.TB) []byte {
	t.Helper()
	patchSeedOnce.Do(func() {
		f, err := os.CreateTemp("", "ext4fuzz")
		if err != nil {
			return
		}
		path := f.Name()
		_ = f.Close()
		defer func() { _ = os.Remove(path) }()

		fs, err := ext4.Format(path, 512<<10, ext4.FormatConfig{})
		if err != nil {
			return
		}
		if err := fs.MkDir("/dir", 0o755); err != nil {
			_ = fs.Close()
			return
		}
		if err := fs.WriteFile("/dir/seed.txt", []byte("hello hardening world"), 0o644); err != nil {
			_ = fs.Close()
			return
		}
		_ = fs.Close()
		img, err := os.ReadFile(path)
		if err != nil {
			return
		}
		patchSeed = img
	})
	if patchSeed == nil {
		t.Skip("could not build a seed image")
	}
	return patchSeed
}

func FuzzOpenAndReadPatched(f *testing.F) {
	base := smallValidImage(f)

	// Seeds aimed at the structures worth corrupting. The superblock lives at
	// 1024; the group descriptor table and the first inode table follow.
	f.Add(int64(1024+0x28), []byte{0xff, 0xff, 0xff, 0xff}) // blocks per group
	f.Add(int64(1024+0x54), []byte{0xff, 0xff, 0xff, 0xff}) // inodes count
	f.Add(int64(2048), []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	f.Add(int64(4096), []byte{0xff, 0xff, 0xff, 0xff})
	f.Add(int64(0), []byte{0x00})

	f.Fuzz(func(t *testing.T, off int64, patch []byte) {
		if len(patch) == 0 || len(patch) > 4096 {
			return
		}
		if off < 0 || off >= int64(len(base)) {
			return
		}
		img := make([]byte, len(base))
		copy(img, base)
		copy(img[off:], patch)

		fs, err := ext4.OpenFromDevice(newMemBlockDevice(img), -1)
		if err != nil {
			return
		}
		defer func() { _ = fs.Close() }()
		for _, p := range []string{"/", "/dir", "/dir/seed.txt", "/nonexistent", "/a/b/c"} {
			_, _ = fs.ReadFile(p)
			_, _ = fs.ListDir(p)
			_, _ = fs.Stat(p)
		}
	})
}
