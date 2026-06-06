package filesystem_ext4_test

import (
	"errors"
	"strings"
	"testing"

	ext4 "github.com/go-filesystems/ext4"
	filesystem "github.com/go-filesystems/interface"
)

// stubFormatFile implements ext4.FormatFile for injecting write/truncate/close failures.
type stubFormatFile struct {
	truncateErr error
	closeErr    error
	writeErrAt  map[int64]error
	writes      map[int64][]byte
	truncatedTo int64
	closed      bool
}

func (f *stubFormatFile) WriteAt(p []byte, off int64) (int, error) {
	if err := f.writeErrAt[off]; err != nil {
		return 0, err
	}
	if f.writes == nil {
		f.writes = make(map[int64][]byte)
	}
	f.writes[off] = append([]byte(nil), p...)
	return len(p), nil
}

func (f *stubFormatFile) Truncate(size int64) error {
	f.truncatedTo = size
	return f.truncateErr
}

func (f *stubFormatFile) Close() error {
	f.closed = true
	return f.closeErr
}

func installFormatHooks(t *testing.T, open func(string) (ext4.FormatFile, error), randRead func([]byte) (int, error), openFS func(string, int) (filesystem.Filesystem, error)) {
	t.Helper()
	oldOpen := ext4.SetFormatOpenFile(nil)
	ext4.SetFormatOpenFile(oldOpen) // restore default then set below
	if open != nil {
		oldOpen = ext4.SetFormatOpenFile(open)
	}
	oldRand := ext4.SetFormatRandRead(nil)
	ext4.SetFormatRandRead(oldRand)
	if randRead != nil {
		oldRand = ext4.SetFormatRandRead(randRead)
	}
	oldOpenFS := ext4.SetFormatOpenFS(nil)
	ext4.SetFormatOpenFS(oldOpenFS)
	if openFS != nil {
		oldOpenFS = ext4.SetFormatOpenFS(openFS)
	}
	t.Cleanup(func() {
		ext4.SetFormatOpenFile(oldOpen)
		ext4.SetFormatRandRead(oldRand)
		ext4.SetFormatOpenFS(oldOpenFS)
	})
}

func TestFormatRemainingPaths(t *testing.T) {
	errBoom := errors.New("boom")

	const fmtBlockSize = 4096
	const fmtInodeTableBlocks = 16

	t.Run("size too small after alignment check", func(t *testing.T) {
		tooSmall := int64((fmtInodeTableBlocks + 4) * fmtBlockSize)
		if _, err := ext4.Format(t.TempDir()+"/tiny.img", tooSmall, ext4.FormatConfig{}); err == nil {
			t.Fatalf("expected Format to reject an aligned image smaller than the minimum")
		}
	})

	t.Run("open error", func(t *testing.T) {
		installFormatHooks(t,
			func(string) (ext4.FormatFile, error) { return nil, errBoom },
			nil,
			nil,
		)
		if _, err := ext4.Format("ignored.img", 20*1024*1024, ext4.FormatConfig{UUID: [16]byte{1}}); err == nil {
			t.Fatalf("expected Format to fail when opening the backing file")
		}
	})

	t.Run("truncate error", func(t *testing.T) {
		stub := &stubFormatFile{truncateErr: errBoom}
		installFormatHooks(t,
			func(string) (ext4.FormatFile, error) { return stub, nil },
			nil,
			nil,
		)
		if _, err := ext4.Format("ignored.img", 20*1024*1024, ext4.FormatConfig{UUID: [16]byte{1}}); err == nil {
			t.Fatalf("expected Format to fail when truncating the backing file")
		}
		if !stub.closed {
			t.Fatalf("expected Format to close the backing file after truncate failure")
		}
	})

	t.Run("uuid generation error", func(t *testing.T) {
		stub := &stubFormatFile{}
		installFormatHooks(t,
			func(string) (ext4.FormatFile, error) { return stub, nil },
			func([]byte) (int, error) { return 0, errBoom },
			nil,
		)
		if _, err := ext4.Format("ignored.img", 20*1024*1024, ext4.FormatConfig{}); err == nil {
			t.Fatalf("expected Format to fail when generating a UUID")
		}
		if !stub.closed {
			t.Fatalf("expected Format to close the backing file after UUID generation failure")
		}
	})

	t.Run("size too large for uint32 block counts", func(t *testing.T) {
		hugeSize := (int64(^uint32(0)) + 1) * fmtBlockSize
		if _, err := ext4.Format("ignored.img", hugeSize, ext4.FormatConfig{UUID: [16]byte{1}}); err == nil {
			t.Fatalf("expected Format to reject an image larger than uint32 block counts can represent")
		}
	})

	t.Run("long label is trimmed before BGDT write failure", func(t *testing.T) {
		stub := &stubFormatFile{writeErrAt: map[int64]error{fmtBlockSize: errBoom}}
		installFormatHooks(t,
			func(string) (ext4.FormatFile, error) { return stub, nil },
			nil,
			nil,
		)
		label := strings.Repeat("x", 20)
		if _, err := ext4.Format("ignored.img", 20*1024*1024, ext4.FormatConfig{UUID: [16]byte{1}, Label: label}); err == nil {
			t.Fatalf("expected Format to fail when writing the BGDT")
		}
		raw := stub.writes[1024]
		if got := string(raw[120:136]); got != strings.Repeat("x", 16) {
			t.Fatalf("superblock label = %q, want %q", got, strings.Repeat("x", 16))
		}
	})

	t.Run("superblock write error", func(t *testing.T) {
		stub := &stubFormatFile{writeErrAt: map[int64]error{1024: errBoom}}
		installFormatHooks(t,
			func(string) (ext4.FormatFile, error) { return stub, nil },
			nil,
			nil,
		)
		if _, err := ext4.Format("ignored.img", 20*1024*1024, ext4.FormatConfig{UUID: [16]byte{1}}); err == nil {
			t.Fatalf("expected Format to fail when writing the superblock")
		}
	})

	t.Run("block bitmap write error", func(t *testing.T) {
		stub := &stubFormatFile{writeErrAt: map[int64]error{2 * fmtBlockSize: errBoom}}
		installFormatHooks(t,
			func(string) (ext4.FormatFile, error) { return stub, nil },
			nil,
			nil,
		)
		if _, err := ext4.Format("ignored.img", 20*1024*1024, ext4.FormatConfig{UUID: [16]byte{1}}); err == nil {
			t.Fatalf("expected Format to fail when writing the block bitmap")
		}
	})

	t.Run("inode bitmap write error", func(t *testing.T) {
		stub := &stubFormatFile{writeErrAt: map[int64]error{3 * fmtBlockSize: errBoom}}
		installFormatHooks(t,
			func(string) (ext4.FormatFile, error) { return stub, nil },
			nil,
			nil,
		)
		if _, err := ext4.Format("ignored.img", 20*1024*1024, ext4.FormatConfig{UUID: [16]byte{1}}); err == nil {
			t.Fatalf("expected Format to fail when writing the inode bitmap")
		}
	})

	t.Run("root inode write error", func(t *testing.T) {
		stub := &stubFormatFile{writeErrAt: map[int64]error{4*fmtBlockSize + 256: errBoom}}
		installFormatHooks(t,
			func(string) (ext4.FormatFile, error) { return stub, nil },
			nil,
			nil,
		)
		if _, err := ext4.Format("ignored.img", 20*1024*1024, ext4.FormatConfig{UUID: [16]byte{1}}); err == nil {
			t.Fatalf("expected Format to fail when writing the root inode")
		}
	})

	t.Run("root directory write error", func(t *testing.T) {
		stub := &stubFormatFile{writeErrAt: map[int64]error{20 * fmtBlockSize: errBoom}}
		installFormatHooks(t,
			func(string) (ext4.FormatFile, error) { return stub, nil },
			nil,
			nil,
		)
		if _, err := ext4.Format("ignored.img", 20*1024*1024, ext4.FormatConfig{UUID: [16]byte{1}}); err == nil {
			t.Fatalf("expected Format to fail when writing the root directory")
		}
	})

	t.Run("close error", func(t *testing.T) {
		stub := &stubFormatFile{closeErr: errBoom}
		installFormatHooks(t,
			func(string) (ext4.FormatFile, error) { return stub, nil },
			nil,
			nil,
		)
		if _, err := ext4.Format("ignored.img", 20*1024*1024, ext4.FormatConfig{UUID: [16]byte{1}}); err == nil {
			t.Fatalf("expected Format to fail when closing the backing file")
		}
	})
}
