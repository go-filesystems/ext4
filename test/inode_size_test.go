// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package filesystem_ext4_test

import (
	"testing"

	ext4 "github.com/go-filesystems/ext4"
)

// s_inode_size sizes every inode buffer, and the fixed field offsets are read
// out of it unconditionally -- i_size_high sits at byte 108. The parser
// defaulted a zero to 128 and accepted everything else, so a six-byte
// corruption of the superblock made readInode panic with
// "slice bounds out of range [0:48]".
func TestSuperblockRejectsAnUndersizedInodeSize(t *testing.T) {
	base := smallValidImage(t)
	img := make([]byte, len(base))
	copy(img, base)
	// The reproducer the fuzzer found: six bytes over s_rev_level and
	// s_inode_size at superblock offset 84.
	copy(img[1108:], []byte("00000\x00"))

	fs, err := ext4.OpenFromDevice(newMemBlockDevice(img), -1)
	if err != nil {
		return // refused at Open is a fine answer; a panic is not
	}
	defer func() { _ = fs.Close() }()
	for _, p := range []string{"/", "/dir", "/dir/seed.txt"} {
		_, _ = fs.ReadFile(p)
		_, _ = fs.ListDir(p)
		_, _ = fs.Stat(p)
	}
}

// The guard must not be refusing valid images: 128- and 256-byte inodes are
// both ordinary, and the image the formatter produces still reads back.
func TestValidImageStillReads(t *testing.T) {
	base := smallValidImage(t)
	img := make([]byte, len(base))
	copy(img, base)

	fs, err := ext4.OpenFromDevice(newMemBlockDevice(img), -1)
	if err != nil {
		t.Fatalf("OpenFromDevice on an unmodified image: %v", err)
	}
	defer func() { _ = fs.Close() }()
	data, err := fs.ReadFile("/dir/seed.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "hello hardening world" {
		t.Errorf("ReadFile = %q, want %q", data, "hello hardening world")
	}
	if _, err := fs.ListDir("/dir"); err != nil {
		t.Errorf("ListDir: %v", err)
	}
}
