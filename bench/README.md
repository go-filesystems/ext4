# Performance benchmarks

Two halves that measure the **same standard operations** so the pure-Go driver
can be read side by side with the in-kernel ext4 implementation.

## Go-driver side (portable, runs anywhere)

```sh
go test -bench=. -benchmem -run='^$'
```

Benchmarks (in `../bench_test.go`, public-API only): `Format`, `WriteFileSeq`,
`ReadFileSeq`, `Stat`, `ListDir`, `CreateFiles`, `DeleteFiles`. File-backed
image under `b.TempDir()` so the numbers include real block I/O.

## Reference side (in-kernel ext4, Linux only, needs root)

```sh
scp bench/compare.sh dc1-r1-h1:/tmp/ && ssh dc1-r1-h1 'sudo bash /tmp/compare.sh'
```

`compare.sh` runs the same ops via `mkfs.ext4` + `mount -o loop` + `dd`
(with `fsync`/`drop_caches`) + coreutils.

> **Caveat — not apples-to-apples.** The kernel has a page cache, journal and
> writeback; the Go driver does synchronous user-space block I/O. Treat the
> kernel numbers as a rough upper-bound reference, not a literal target.

## First findings (2026-06, Apple M4 Max vs weft VM kernel)

| Operation        | go-filesystems/ext4   | kernel ext4 (ref) |
|------------------|-----------------------|-------------------|
| Sequential read  | ~1.8–4.8 GB/s         | ~3.2 GB/s         |
| Sequential write | ~2.6–7 MB/s           | ~975 MB/s         |
| Create file      | ~12 ms/file           | ~0.075 ms/file    |
| Delete file      | ~20 ms/file           | ~0.4 ms/file      |
| Format           | ~9.7 ms               | ~1 ms             |
| Stat             | ~0.4–2 ms             | ~0.5 ms           |

**Reads are competitive; every mutation (write/create/delete) is 1–2 orders of
magnitude slower and allocation-heavy** (~340k allocs for an 8 MiB write). The
cost is almost certainly per-operation metadata rewrites (bitmaps / inode table
/ superblock) instead of batching dirty blocks — an algorithmic write-path
issue, identical across all target architectures (no SIMD involved).

This is the top optimization target; profile with
`go test -bench=BenchmarkWriteFileSeq -cpuprofile=cpu.out -memprofile=mem.out`.
