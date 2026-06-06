// stress_test.go — intensive concurrent stress tests for the ext4 public API.
//
// Each image (Debian, Ubuntu) is exercised with tens of thousands of
// simultaneous read / write / listdir / stat operations spread across many
// goroutines.
//
// All tests are skipped when the target image is not present in ~/.mock/cache.
package filesystem_ext4_test

import (
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	disk_qcow2 "github.com/go-diskimages/qcow2"
	ext4 "github.com/go-filesystems/ext4"
)

// ──────────────────── image catalogue ──────────────────────────────────────

// stressImageSpec describes one cloud image that may be present in the mock
// cache. format is "raw" (already a raw disk), "qcow2", or "img" (qcow2
// with .img extension, as shipped by Ubuntu).
type stressImageSpec struct {
	distro     string
	format     string   // "raw" | "qcow2" | "img"
	candidates []string // relative paths under ~/.mock/cache
}

// allStressImages lists the images exercised by the ext4 stress suite.
var allStressImages = []stressImageSpec{
	{
		distro: "debian",
		format: "raw",
		candidates: []string{
			// Debian 13 (Trixie) aarch64
			"https____cloud.debian.org_images_cloud_trixie_latest_debian-13-genericcloud-arm64.raw/debian-13-genericcloud-arm64.raw",
			// Debian 12 (Bookworm) aarch64
			"https____cloud.debian.org_images_cloud_bookworm_latest_debian-12-genericcloud-arm64.raw/debian-12-genericcloud-arm64.raw",
			// Debian 13 amd64
			"https____cloud.debian.org_images_cloud_trixie_latest_debian-13-genericcloud-amd64.raw/debian-13-genericcloud-amd64.raw",
			// Debian 12 amd64
			"https____cloud.debian.org_images_cloud_bookworm_latest_debian-12-genericcloud-amd64.raw/debian-12-genericcloud-amd64.raw",
		},
	},
	{
		distro: "ubuntu",
		format: "img", // Ubuntu ships qcow2 with .img extension
		candidates: []string{
			// Ubuntu 24.04 (Noble) aarch64
			"https____cloud-images.ubuntu.com_noble_current_noble-server-cloudimg-arm64.img/noble-server-cloudimg-arm64.img",
			// Ubuntu 24.04 amd64
			"https____cloud-images.ubuntu.com_noble_current_noble-server-cloudimg-amd64.img/noble-server-cloudimg-amd64.img",
			// Ubuntu 22.04 (Jammy) aarch64
			"https____cloud-images.ubuntu.com_jammy_current_jammy-server-cloudimg-arm64.img/jammy-server-cloudimg-arm64.img",
			// Ubuntu 22.04 amd64
			"https____cloud-images.ubuntu.com_jammy_current_jammy-server-cloudimg-amd64.img/jammy-server-cloudimg-amd64.img",
		},
	},
}

// ──────────────────── image resolution helpers ─────────────────────────────

// resolveStressImage tries each candidate path under ~/.mock/cache and returns
// the absolute path to the first image that exists. Returns "" if none found.
// Falls back to a fuzzy scan of the cache directory by distro keyword.
func resolveStressImage(t *testing.T, spec stressImageSpec) string {
	home := os.Getenv("HOME")
	cacheDir := filepath.Join(home, ".mock", "cache")

	for _, rel := range spec.candidates {
		p := filepath.Join(cacheDir, filepath.FromSlash(rel))
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}

	// Not present — attempt to pull/download each candidate into the cache.
	for _, rel := range spec.candidates {
		// rel uses forward slashes; split to encoded folder + filename.
		idx := strings.LastIndex(rel, "/")
		var enc, fname string
		if idx == -1 {
			enc = rel
			fname = rel
		} else {
			enc = rel[:idx]
			fname = rel[idx+1:]
		}
		if got, err := ensureImageInCache(t, enc, fname); err == nil {
			return got
		} else {
			// keep trying others
			continue
		}
	}

	// Fuzzy fallback: scan cache for any directory whose name contains the
	// distro keyword and return the first matching ext inside.
	entries, _ := os.ReadDir(cacheDir)
	for _, e := range entries {
		if !e.IsDir() || !strings.Contains(strings.ToLower(e.Name()), spec.distro) {
			continue
		}
		subs, _ := os.ReadDir(filepath.Join(cacheDir, e.Name()))
		for _, f := range subs {
			name := f.Name()
			if strings.HasSuffix(name, ".raw") ||
				strings.HasSuffix(name, ".img") ||
				strings.HasSuffix(name, ".qcow2") {
				return filepath.Join(cacheDir, e.Name(), name)
			}
		}
	}
	return ""
}

// toStressRaw converts a qcow2 / .img file to a raw image that lives alongside
// the source and is reused across runs. Already-raw inputs are returned as-is.
func toStressRaw(t *testing.T, src string, format string) string {
	t.Helper()
	if format == "raw" {
		return src
	}
	raw := strings.TrimSuffix(src, filepath.Ext(src)) + "-ext4stress.raw"
	qi, _ := os.Stat(src)
	ri, rerr := os.Stat(raw)
	if rerr != nil || (qi != nil && ri.ModTime().Before(qi.ModTime())) {
		t.Logf("converting %s → raw", filepath.Base(src))
		if err := disk_qcow2.ConvertToRaw(src, raw, os.Stdout); err != nil {
			t.Fatalf("disk_qcow2.ConvertToRaw: %v", err)
		}
	}
	return raw
}

// copyStressImage copies src into a fresh temp file for write isolation.
func copyStressImage(t *testing.T, src string) string {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatalf("open image: %v", err)
	}
	defer in.Close()
	out, err := os.CreateTemp(t.TempDir(), "ext4stress-*.raw")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		t.Fatalf("copy image: %v", err)
	}
	if err := out.Close(); err != nil {
		t.Fatalf("close temp: %v", err)
	}
	return out.Name()
}

// findExt4Partition tries auto-detect then explicit indices 0–7 to find a
// readable ext4 partition. Skips the test if none found.
func findExt4Partition(t *testing.T, rawPath string) int {
	t.Helper()
	// Probe on a temporary copy so the original image is never opened for
	// read-write by the test harness.
	tmp := copyImage(t, rawPath)
	if fs, err := ext4.Open(tmp, -1); err == nil {
		fs.Close()
		return -1
	}
	for i := 0; i < 8; i++ {
		if fs, err := ext4.Open(tmp, i); err == nil {
			fs.Close()
			return i
		}
	}
	t.Skipf("no ext4 partition found in image %s", filepath.Base(rawPath))
	return -1
}

// ──────────────────── stress constants ─────────────────────────────────────

var (
	ext4StressWorkers = func() int {
		if v := os.Getenv("EXT4_STRESS_WORKERS"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				return n
			}
		}
		return 32
	}()

	ext4StressOpsPerWorker = func() int {
		if v := os.Getenv("EXT4_STRESS_OPS_PER_WORKER"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				return n
			}
		}
		return 250 // 32 × 250 = 8,000 ops per run
	}()
)

// ──────────────────── mixed stress kernel ──────────────────────────────────

// stressExt4Image runs the mixed concurrent stress suite against the writable
// copy of one raw image.
func stressExt4Image(t *testing.T, rawPath string, partIdx int) {
	t.Helper()

	tmp := copyStressImage(t, rawPath)
	fs, err := ext4.Open(tmp, partIdx)
	if err != nil {
		t.Fatalf("ext4.Open: %v", err)
	}
	t.Cleanup(func() { fs.Close() })

	staticReadPaths := []string{
		"/etc/os-release",
		"/etc/hosts",
		"/etc/fstab",
		"/etc/passwd",
		"/etc/shadow",
		"/etc/group",
		"/etc/shells",
	}
	staticDirs := []string{
		"/etc",
		"/usr",
		"/var",
		"/",
	}

	type opKind int
	const (
		opRead opKind = iota
		opStat
		opListDir
		opWrite
		opMax
	)

	var (
		wg         sync.WaitGroup
		opsOK      atomic.Int64
		opsErr     atomic.Int64
		firstErrMu sync.Mutex
		firstErr   error
	)

	recordErr := func(err error) {
		opsErr.Add(1)
		firstErrMu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		firstErrMu.Unlock()
	}

	for w := 0; w < ext4StressWorkers; w++ {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			localRng := rand.New(rand.NewPCG(uint64(w)*0x100000000+0xdeadbeef, uint64(w)))

			for i := 0; i < ext4StressOpsPerWorker; i++ {
				op := opKind(localRng.IntN(int(opMax)))
				switch op {
				case opRead:
					path := staticReadPaths[localRng.IntN(len(staticReadPaths))]
					data, err := fs.ReadFile(path)
					if err != nil {
						if !ext4IsNotFound(err) {
							recordErr(fmt.Errorf("ReadFile(%q): %w", path, err))
						}
					} else if len(data) == 0 {
						recordErr(fmt.Errorf("ReadFile(%q): empty", path))
					} else {
						opsOK.Add(1)
					}

				case opStat:
					path := staticReadPaths[localRng.IntN(len(staticReadPaths))]
					st, err := fs.Stat(path)
					if err != nil {
						if !ext4IsNotFound(err) {
							recordErr(fmt.Errorf("Stat(%q): %w", path, err))
						}
					} else if st.Inode() == 0 {
						recordErr(fmt.Errorf("Stat(%q): inode=0", path))
					} else {
						opsOK.Add(1)
					}

				case opListDir:
					dir := staticDirs[localRng.IntN(len(staticDirs))]
					entries, err := fs.ListDir(dir)
					if err != nil {
						recordErr(fmt.Errorf("ListDir(%q): %w", dir, err))
					} else if len(entries) == 0 {
						recordErr(fmt.Errorf("ListDir(%q): 0 entries", dir))
					} else {
						opsOK.Add(1)
					}

				case opWrite:
					path := fmt.Sprintf("/etc/ext4stress-w%d-i%d.txt", w, i)
					payload := fmt.Appendf(nil, "worker=%d iter=%d\n", w, i)
					if err := fs.WriteFile(path, payload, 0o644); err != nil {
						recordErr(fmt.Errorf("WriteFile(%q): %w", path, err))
					} else {
						opsOK.Add(1)
					}
				}
			}
		}()
	}

	wg.Wait()
	t.Logf("mixed stress: %d ok, %d errors (workers=%d ops/worker=%d)",
		opsOK.Load(), opsErr.Load(), ext4StressWorkers, ext4StressOpsPerWorker)
	if firstErr != nil {
		t.Errorf("first error: %v", firstErr)
	}
}

func ext4IsNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") ||
		strings.Contains(msg, "no such") ||
		strings.Contains(msg, "enoent")
}

// ──────────────────── top-level per-image tests ────────────────────────────

func TestStress_Debian(t *testing.T) {
	spec := allStressImages[0]
	src := resolveStressImage(t, spec)
	if src == "" {
		t.Skipf("no %s image found in mock cache", spec.distro)
	}
	raw := toStressRaw(t, src, spec.format)
	partIdx := findExt4Partition(t, raw)
	stressExt4Image(t, raw, partIdx)
}

func TestStress_Ubuntu(t *testing.T) {
	spec := allStressImages[1]
	src := resolveStressImage(t, spec)
	if src == "" {
		t.Skipf("no %s image found in mock cache", spec.distro)
	}
	raw := toStressRaw(t, src, spec.format)
	partIdx := findExt4Partition(t, raw)
	stressExt4Image(t, raw, partIdx)
}

// ──────────────────── TestStress_Concurrent ────────────────────────────────

func TestStress_Concurrent(t *testing.T) {
	_ = runtime.GOMAXPROCS(0)

	for _, spec := range allStressImages {
		spec := spec
		src := resolveStressImage(t, spec)
		if src == "" {
			t.Logf("skipping %s: no image found in cache", spec.distro)
			continue
		}

		raw := strings.TrimSuffix(src, filepath.Ext(src)) + "-ext4stress.raw"
		if spec.format == "raw" {
			raw = src
		} else {
			if _, err := os.Stat(raw); err != nil {
				t.Logf("converting %s → raw…", filepath.Base(src))
				if err := disk_qcow2.ConvertToRaw(src, raw, os.Stdout); err != nil {
					t.Fatalf("disk_qcow2.ConvertToRaw: %v", err)
				}
			}
		}

		partIdx := -1
		if fs, err := ext4.Open(raw, -1); err == nil {
			fs.Close()
		} else {
			found := false
			for i := 0; i < 8; i++ {
				if fs, err := ext4.Open(raw, i); err == nil {
					fs.Close()
					partIdx = i
					found = true
					break
				}
			}
			if !found {
				t.Logf("skipping %s: no ext4 partition found", spec.distro)
				continue
			}
		}
		capturedPartIdx := partIdx
		capturedRaw := raw

		t.Run(spec.distro+"/concurrent_rw", func(t *testing.T) {
			stressExt4Image(t, capturedRaw, capturedPartIdx)
		})

		t.Run(spec.distro+"/read_only", func(t *testing.T) {
			fs, err := ext4.Open(capturedRaw, capturedPartIdx)
			if err != nil {
				t.Fatalf("ext4.Open: %v", err)
			}
			defer fs.Close()

			workers := ext4StressWorkers
			iters := ext4StressOpsPerWorker

			staticPaths := []string{
				"/etc/os-release",
				"/etc/passwd",
				"/etc/group",
				"/etc/fstab",
				"/etc/hosts",
			}
			staticDirs := []string{"/etc", "/usr", "/var", "/"}

			var wg sync.WaitGroup
			var errCount atomic.Int64
			var firstErrMu sync.Mutex
			var firstErr error

			for w := 0; w < workers; w++ {
				w := w
				wg.Add(1)
				go func() {
					defer wg.Done()
					localRng := rand.New(rand.NewPCG(uint64(w), 0xfeedface))
					for i := 0; i < iters; i++ {
						if w%2 == 0 {
							path := staticPaths[localRng.IntN(len(staticPaths))]
							if _, err := fs.ReadFile(path); err != nil && !ext4IsNotFound(err) {
								errCount.Add(1)
								firstErrMu.Lock()
								if firstErr == nil {
									firstErr = fmt.Errorf("ReadFile(%q): %w", path, err)
								}
								firstErrMu.Unlock()
							}
						} else {
							dir := staticDirs[localRng.IntN(len(staticDirs))]
							entries, err := fs.ListDir(dir)
							if err != nil {
								errCount.Add(1)
								firstErrMu.Lock()
								if firstErr == nil {
									firstErr = fmt.Errorf("ListDir(%q): %w", dir, err)
								}
								firstErrMu.Unlock()
							} else if len(entries) == 0 {
								errCount.Add(1)
								firstErrMu.Lock()
								if firstErr == nil {
									firstErr = fmt.Errorf("ListDir(%q): 0 entries", dir)
								}
								firstErrMu.Unlock()
							}
						}
					}
				}()
			}
			wg.Wait()
			t.Logf("read_only: %d errors / %d ops", errCount.Load(), workers*iters)
			if firstErr != nil {
				t.Errorf("first error: %v", firstErr)
			}
		})

		t.Run(spec.distro+"/write_hammer", func(t *testing.T) {
			// Each goroutine works on its own writable copy to avoid
			// cross-worker contention on the same inode/block structures.
			const workers = 8
			const writesPerWorker = 600 // 8 × 600 = 4,800 write+readback cycles

			var wg sync.WaitGroup
			var errCount atomic.Int64
			var firstErrMu sync.Mutex
			var firstErr error

			for w := 0; w < workers; w++ {
				w := w
				wg.Add(1)
				go func() {
					defer wg.Done()
					tmp := copyStressImage(t, capturedRaw)
					fs, err := ext4.Open(tmp, capturedPartIdx)
					if err != nil {
						errCount.Add(1)
						firstErrMu.Lock()
						if firstErr == nil {
							firstErr = fmt.Errorf("worker %d open: %v", w, err)
						}
						firstErrMu.Unlock()
						return
					}
					defer fs.Close()

					for i := 0; i < writesPerWorker; i++ {
						path := fmt.Sprintf("/etc/ext4hammer-w%d-i%06d.txt", w, i)
						payload := fmt.Appendf(nil, "w=%d i=%d\n", w, i)

						if err := fs.WriteFile(path, payload, 0o644); err != nil {
							errCount.Add(1)
							firstErrMu.Lock()
							if firstErr == nil {
								firstErr = fmt.Errorf("WriteFile(%q): %w", path, err)
							}
							firstErrMu.Unlock()
							return
						}

						// Verify readback integrity.
						got, err := fs.ReadFile(path)
						if err != nil {
							errCount.Add(1)
							firstErrMu.Lock()
							if firstErr == nil {
								firstErr = fmt.Errorf("ReadFile(%q) after write: %w", path, err)
							}
							firstErrMu.Unlock()
							return
						}
						if string(got) != string(payload) {
							errCount.Add(1)
							firstErrMu.Lock()
							if firstErr == nil {
								firstErr = fmt.Errorf("ReadFile(%q): got %q, want %q", path, got, payload)
							}
							firstErrMu.Unlock()
							return
						}
					}
				}()
			}
			wg.Wait()
			t.Logf("write_hammer: %d errors / %d write+readback cycles",
				errCount.Load(), workers*writesPerWorker)
			if firstErr != nil {
				t.Errorf("first error: %v", firstErr)
			}
		})
	}
}
