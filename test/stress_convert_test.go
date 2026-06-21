// stress_convert_test.go — qemu-img-based default implementation of the
// stress-test image converter.
//
// The stress / image round-trip tests need to turn a qcow2 (or Ubuntu .img)
// cloud image into a raw disk before opening it with the ext4 driver. This
// qemu-img shell-out is the default converter (mirroring ext4_test.go's Rocky
// path) and is compiled in every build, so the package builds without checking
// out the github.com/go-diskimages/qcow2 sibling module — exactly the
// situation in the CI cross-compile / vet steps. Under the `stress` build tag,
// stress_qcow2_test.go's init() overrides stressConvertToRaw with the native
// pure-Go converter from that sibling module.
//
// All of these tests skip when no image is present in ~/.mock/cache (the CI
// condition), so this fallback only ever runs on a developer machine that has
// both the cached image and qemu-img installed.
package filesystem_ext4_test

import (
	"fmt"
	"io"
	"os"
	"os/exec"
)

// stressConvertToRawDefault converts src (qcow2 / .img) to a raw image at dst
// using qemu-img. Progress is written to w (unused by qemu-img but kept for
// signature compatibility with the native converter).
func stressConvertToRawDefault(src, dst string, w io.Writer) error {
	qemuImg, err := exec.LookPath("qemu-img")
	if err != nil {
		for _, c := range []string{
			"/opt/homebrew/bin/qemu-img",
			"/usr/local/bin/qemu-img",
			"/usr/bin/qemu-img",
		} {
			if _, statErr := os.Stat(c); statErr == nil {
				qemuImg = c
				err = nil
				break
			}
		}
	}
	if err != nil {
		return fmt.Errorf("qemu-img not found: %w", err)
	}

	cmd := exec.Command(qemuImg, "convert", "-O", "raw", src, dst)
	if out, runErr := cmd.CombinedOutput(); runErr != nil {
		if w != nil {
			fmt.Fprintf(w, "%s", out)
		}
		return fmt.Errorf("qemu-img convert %s -> %s: %w\n%s", src, dst, runErr, out)
	}
	return nil
}
