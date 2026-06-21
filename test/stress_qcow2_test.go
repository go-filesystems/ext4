//go:build stress
// +build stress

// stress_qcow2_test.go — native (pure-Go) image converter for the stress build.
//
// Under the `stress` build tag we use the github.com/go-diskimages/qcow2
// sibling module's pure-Go ConvertToRaw, the same converter the rest of the
// go-filesystems family relies on. This file is the *only* place the qcow2
// module is imported, so the default build (and the CI cross-compile / vet
// steps, which do not check out that sibling) never pulls it into the build
// graph. Run with `task test-stress` (`go test -tags=test,stress ./...`).
package filesystem_ext4_test

import (
	"io"

	disk_qcow2 "github.com/go-diskimages/qcow2"
)

func init() {
	stressConvertToRaw = func(src, dst string, w io.Writer) error {
		return disk_qcow2.ConvertToRaw(src, dst, w)
	}
}
