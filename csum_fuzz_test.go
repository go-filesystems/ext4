package filesystem_ext4

import (
	"errors"
	"testing"
)

// FuzzChecksumVerify feeds arbitrary bytes to every metadata_csum verifier.
// Contract: never panic, and any refusal is a *ChecksumError usable with
// errors.As. The corpus under testdata/fuzz/FuzzChecksumVerify is committed;
// it holds the shapes that matter (empty, one-byte, a directory tail, a
// forged htree count, an inode-sized buffer, a descriptor-sized buffer).
func FuzzChecksumVerify(f *testing.F) {
	f.Add([]byte{}, uint32(2), uint8(0))
	f.Add([]byte{0xFF}, uint32(0), uint8(1))
	f.Add(make([]byte, 12), uint32(2), uint8(2))
	f.Add(make([]byte, 128), uint32(11), uint8(3))
	f.Add(make([]byte, 256), uint32(2), uint8(0))
	f.Add(make([]byte, 4096), uint32(2), uint8(1))
	f.Fuzz(func(t *testing.T, buf []byte, num uint32, shape uint8) {
		sb := &superblock{
			FeatureROCompat: FeatROCompatMetadataCsum,
			DescSize:        32 + 32*uint16(shape&1),
			BlockSize:       1024 << (shape % 3),
			BlocksPerGroup:  8192,
			InodesPerGroup:  2048,
			InodeSize:       128 << (shape & 1),
		}
		for _, err := range []error{
			verifyDirBlockCsum(sb, num, num^0x5a5a, uint64(num), buf),
			verifyInodeCsum(sb, num, buf),
			verifyBGDCsum(sb, num%64, buf),
			verifyBitmapCsum(sb, num%64, &bgd{raw: make([]byte, 64)}, shape&2 == 0, buf),
		} {
			if err == nil {
				continue
			}
			var ce *ChecksumError
			if !errors.As(err, &ce) || !errors.Is(err, ErrChecksum) {
				t.Fatalf("refusal %v is not a ChecksumError", err)
			}
		}
	})
}
