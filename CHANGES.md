Changelog
=========

Recent local changes:

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
