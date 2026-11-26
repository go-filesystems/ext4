package filesystem_ext4

import (
	"fmt"
	"io"
	"os"
	"sync"
)

// The library never writes diagnostics to the filesystem: a fixed or
// predictable path in a shared directory is open to symlink and
// pre-creation attacks, leaks volume contents and grows without bound.
// Diagnostics go to a bounded in-memory buffer that a caller reads with
// DumpDebugLog; they are recorded only when EXT4_LOCK_DEBUG=1.

var (
	debugOnce    sync.Once
	debugMu      sync.Mutex
	debugBuf     []byte
	debugEnabled bool
	debugMax     = 10 * 1024 * 1024 // keep last 10MB of debug output
)

func initDebug() {
	debugEnabled = os.Getenv("EXT4_LOCK_DEBUG") == "1"
}

// debugPrintf records formatted debug output into an in-memory circular
// buffer to avoid blocking hot code paths on stdout/stderr. Callers should
// only use this when diagnostics are enabled via EXT4_LOCK_DEBUG.
func debugPrintf(format string, a ...interface{}) {
	debugOnce.Do(initDebug)
	if !debugEnabled {
		return
	}
	s := fmt.Sprintf(format, a...)
	debugMu.Lock()
	defer debugMu.Unlock()
	if len(debugBuf)+len(s) > debugMax {
		// drop the oldest bytes to keep buffer within limit
		over := len(debugBuf) + len(s) - debugMax
		if over >= len(debugBuf) {
			debugBuf = []byte(s)
			return
		}
		debugBuf = append(debugBuf[over:], s...)
		return
	}
	debugBuf = append(debugBuf, s...)
}

// DumpDebugLog writes the accumulated debug buffer to the provided writer.
func DumpDebugLog(w io.Writer) {
	debugMu.Lock()
	defer debugMu.Unlock()
	if len(debugBuf) == 0 {
		return
	}
	_, _ = w.Write(debugBuf)
}

// appendAssocLog records a timing-sensitive instrumentation line in the
// in-memory buffer. Only active when debug is enabled.
func appendAssocLog(s string) {
	debugPrintf("%s\n", s)
}

// appendImmediate records a short instrumentation line in the in-memory
// buffer; tag names the source. It never touches the filesystem.
func appendImmediate(tag, s string) {
	debugPrintf("[%s] %s\n", tag, s)
}
