// copy_helpers_test.go — fast file-clone helpers shared across tests.
//
// The stress and round-trip tests need a private writable copy of a multi-GB
// raw disk image. A naive io.Copy materialises every byte: on APFS / btrfs /
// xfs that wastes seconds per copy and serialises any concurrent fan-out of
// per-worker images. cloneFile prefers a copy-on-write reflink (clonefile on
// darwin, --reflink=auto on linux) and falls back to byte streaming when
// the host filesystem does not support CoW or the cp binary is missing.
package filesystem_ext4_test

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
)

// cloneFile copies src to dst, preferring a copy-on-write clone where the
// host filesystem supports it. dst must not yet exist. The fallback path
// is a plain byte-stream copy.
//
// Errors from the CoW path are silently downgraded to the byte-stream
// fallback: a kernel that refuses the reflink (e.g. cross-filesystem
// boundary, source not on a CoW volume) must not break the test.
func cloneFile(src, dst string) error {
	if err := cowClone(src, dst); err == nil {
		return nil
	}
	// Remove any partial output left behind by a failed CoW attempt.
	_ = os.Remove(dst)
	return streamCopy(src, dst)
}

// cowClone attempts a single copy-on-write clone via the system cp(1) binary.
// Using cp keeps the helper pure-Go-buildable (CGO=0) while still benefitting
// from the kernel fast path. Returns an error when:
//   - cp is not on PATH,
//   - the CoW flag is unsupported on this platform, or
//   - the kernel rejects the reflink request.
func cowClone(src, dst string) error {
	cp, err := exec.LookPath("cp")
	if err != nil {
		return fmt.Errorf("cp not found: %w", err)
	}
	var args []string
	switch runtime.GOOS {
	case "darwin":
		// -c forces clonefile(2); fails if source is not on the same APFS
		// volume as the destination.
		args = []string{"-c", src, dst}
	case "linux":
		// --reflink=auto attempts a btrfs/xfs reflink and silently falls back
		// to a regular copy when unavailable. We still measure throughput so
		// that a non-CoW host does not pretend to be fast.
		args = []string{"--reflink=auto", src, dst}
	default:
		return fmt.Errorf("CoW unsupported on %s", runtime.GOOS)
	}
	cmd := exec.Command(cp, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("cp %v: %v (%s)", args, err, out)
	}
	return nil
}

// streamCopy is the portable, slow fallback used when cowClone is unavailable.
func streamCopy(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		_ = os.Remove(dst)
		return err
	}
	return out.Close()
}
