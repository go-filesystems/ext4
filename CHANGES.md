Changelog
=========

Recent local changes:

- DeleteFile scrubs the inode it frees (PATCH): when the last link goes, the
  inode now gets links 0, an `i_dtime`, size 0, no blocks and an empty extent
  tree, and the blocks of a multi-level extent tree (index and leaf blocks)
  are freed with the data blocks. Before, `e2fsck -fn` reported the freed
  inodes as unattached and their blocks as in use. Removing one name of a
  hardlinked file only decrements the link count (now covered by a test).

- Unused-inode count and uninit flags (PATCH, fixes #50): on a `metadata_csum`
  or `uninit_bg` filesystem the writer now maintains `bg_itable_unused` and
  `bg_flags` (the descriptor was only carrying them through unread). The first
  inode allocated in a group clears `INODE_UNINIT` and lowers the unused count
  to the inodes above it, the first block allocated clears `BLOCK_UNINIT`, and
  the bitmaps of uninitialised groups are read as the kernel synthesises them
  (padding bits and group overhead set), so `e2fsck -fn` no longer reports
  invalid unused-inode counts or uninit-flag errors on images this package wrote.

- Extent block checksums (MINOR, behaviour change, fixes #46): the writer
  now stamps the `ext4_extent_tail` checksum on every extent block it
  creates (file growth, truncation, rewrite, directory growth), and the reader
  verifies it, returning `*ChecksumError` with `Kind` `"extent block"`;
  `Options.IgnoreChecksums` turns it off. An image with a tampered extent
  block, or one written by an earlier version of this package on a
  `metadata_csum` filesystem (extent blocks unstamped), is now refused; read
  it with `IgnoreChecksums` and rewrite the affected files. Inode checksums
  were audited on the write, symlink and link paths: all go through
  `writeInode`/`addInodeToTx`, which stamp them.

- metadata_csum verification (MINOR, behaviour change): superblock, group
  descriptor, inode, directory/htree and bitmap checksums are now verified
  on read and a mismatch returns `*ChecksumError` (`ErrChecksum`). Images
  with a stale checksum that opened before are now refused; use
  `Options.IgnoreChecksums` to read them. Writers now stamp the group
  descriptor checksum after every allocator counter update and the inode
  checksum of new directories, and the directory-block tail is kept
  consistent so a re-written block verifies. Extent block checksums remain
  unverified (follow-up).

- Restore test-only fallback counter (`recordBlockingFallback`) guarded
  by `//go:build test` so diagnostics compile into test builds.
- Shorten journal critical sections in `Transaction.Commit()` by reserving
  append offsets and serializing only `fsync` operations.
- Allocation and metadata locking improvements: try-lock with backoff,
  deterministic blocking fallback, and reduced BGD/bitmap contention.
- Added concurrency regression tests under `pkg/go-filesystems/ext4/test`.

These notes are intended to satisfy the repository documentation policy
requiring a package-level docs update when modifying package behavior.

Local changes (detailed):

- `journal.go`: add background sync workers to coalesce `Sync()` syscalls
  and shorten commit critical sections.
- `fs.go`: close the `Journal` cleanly so workers are stopped on `Close()`.
- `debug_log.go`: add buffered diagnostic logger to avoid blocking syscalls
  in hot paths.
- `locks.go`, `metadata_locks.go`, `inode_table_locks.go`, `trace_debug.go`,
  `write.go`, `fallback_counters.go`: replace blocking prints with
  non-blocking diagnostics and test-only counters.

These entries document the behavioral and observability changes made to the
package and should satisfy the repository's commit documentation checks.
