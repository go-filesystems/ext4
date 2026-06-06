package filesystem_ext4_test

// Shared in-memory helpers to avoid duplicate type definitions across tests.
type memFile struct{ buf []byte }

func (m *memFile) ReadAt(p []byte, off int64) (int, error) {
	n := copy(p, m.buf[off:])
	return n, nil
}

func (m *memFile) WriteAt(p []byte, off int64) (int, error) {
	if int(off)+len(p) > len(m.buf) {
		grow := make([]byte, int(off)+len(p))
		copy(grow, m.buf)
		m.buf = grow
	}
	n := copy(m.buf[off:], p)
	return n, nil
}

type memReaderAt []byte

func (m memReaderAt) ReadAt(p []byte, off int64) (int, error) {
	copy(p, m[off:])
	return len(p), nil
}

// errRW is a small reader/writer that returns configured errors. Placed here
// to avoid duplicate declarations across external tests.
type errRW struct {
	readErr  error
	writeErr error
}

func (e *errRW) ReadAt(p []byte, off int64) (int, error)  { return 0, e.readErr }
func (e *errRW) WriteAt(p []byte, off int64) (int, error) { return 0, e.writeErr }
