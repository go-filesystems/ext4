package filesystem_ext4

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/binary"
	"errors"
	"io"
	iofs "io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	filesystem "github.com/go-filesystems/interface"
)

// The fixtures are images made by the reference tools: mke2fs (e2fsprogs 1.47.4)
// lays the filesystem down and debugfs `set_inode_field` writes every time
// field below, so the expected values are the ones the oracle was told, not
// ones this package computed. Regenerate with EXT4_REGEN_TIMES=1 (needs both).
//
// The images are embedded: the emulated lanes run the test binary away from
// the source tree, where a relative testdata path does not exist.
const (
	times256Fixture = "testdata/times-ext4-i256.img.gz"
	times128Fixture = "testdata/times-ext2-i128.img.gz"
)

//go:embed testdata/times-ext4-i256.img.gz
var times256Gz []byte

//go:embed testdata/times-ext2-i128.img.gz
var times128Gz []byte

// ext4 time fields: seconds (debugfs "@secs"), raw _extra word.
type timesSpec struct {
	file                       string
	a, m, c, cr                string // debugfs time arguments
	aex, mex, cex, crex        string // raw extra words ("" = leave)
	extraIsize                 string // "" = leave
	wantA, wantM, wantC, wantB time.Time
	resSec                     time.Duration // expected resolution of a/m/c
}

func nsExtra(ns, epoch uint32) string { return itoa(ns<<2 | epoch) }

func itoa(v uint32) string {
	if v == 0 {
		return "0"
	}
	var b [10]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

var times256Specs = []timesSpec{
	{
		file: "/full.txt",
		a:    "@1700000000", aex: nsExtra(111111111, 0), wantA: time.Unix(1700000000, 111111111),
		m: "@1715953530", mex: nsExtra(123456789, 0), wantM: time.Unix(1715953530, 123456789),
		c: "@1715953531", cex: nsExtra(5, 0), wantC: time.Unix(1715953531, 5),
		cr: "@1600000000", crex: nsExtra(999999999, 0), wantB: time.Unix(1600000000, 999999999),
		resSec: time.Nanosecond,
	},
	{
		// 2100-01-01 is 4102444800 = 0x1_8 ... : epoch bits 1, base word
		// 4102444800 - 2^32 as a signed 32-bit value.
		file: "/late.txt",
		a:    "@4102444800", aex: nsExtra(7, 1), wantA: time.Unix(4102444800, 7),
		m: "@4102444800", mex: nsExtra(8, 1), wantM: time.Unix(4102444800, 8),
		c: "@4102444800", cex: nsExtra(9, 1), wantC: time.Unix(4102444800, 9),
		cr: "@4102444800", crex: nsExtra(10, 1), wantB: time.Unix(4102444800, 10),
		resSec: time.Nanosecond,
	},
	{
		// extra_isize 4: the inode announces nothing past byte 132, so no
		// _extra word and no crtime fit: whole seconds, no Btime.
		file: "/short.txt",
		a:    "@1700000000", wantA: time.Unix(1700000000, 0),
		m: "@1715953530", wantM: time.Unix(1715953530, 0),
		c: "@1715953531", wantC: time.Unix(1715953531, 0),
		cr:         "@1600000000",
		extraIsize: "4",
		resSec:     time.Second,
	},
	{
		// crtime never set: Btime must be unrecorded, the rest nanosecond.
		file: "/nocr.txt",
		a:    "@1700000000", aex: nsExtra(1, 0), wantA: time.Unix(1700000000, 1),
		m: "@1715953530", mex: nsExtra(2, 0), wantM: time.Unix(1715953530, 2),
		c: "@1715953531", cex: nsExtra(3, 0), wantC: time.Unix(1715953531, 3),
		cr: "0", crex: "0",
		resSec: time.Nanosecond,
	},
}

func e2tool(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, d := range []string{"/opt/homebrew/opt/e2fsprogs/sbin", "/opt/homebrew/sbin", "/sbin", "/usr/sbin", "/usr/local/sbin"} {
		p := filepath.Join(d, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func buildTimesImage(t *testing.T, path string, inodeSize string, fstype string, specs []timesSpec) {
	t.Helper()
	mke2fs, debugfs := e2tool("mke2fs"), e2tool("debugfs")
	if mke2fs == "" || debugfs == "" {
		t.Skip("mke2fs/debugfs not installed")
	}
	src := t.TempDir()
	for _, s := range specs {
		if err := os.WriteFile(filepath.Join(src, filepath.Base(s.file)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("full.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(mke2fs, "-q", "-F", "-t", fstype, "-I", inodeSize, "-b", "1024", "-d", src, path, "2048").CombinedOutput(); err != nil {
		t.Fatalf("mke2fs: %v\n%s", err, out)
	}
	var cmds string
	set := func(file, field, val string) {
		if val != "" {
			cmds += "set_inode_field " + file + " " + field + " " + val + "\n"
		}
	}
	for _, s := range specs {
		set(s.file, "extra_isize", s.extraIsize)
		set(s.file, "atime", s.a)
		set(s.file, "mtime", s.m)
		set(s.file, "ctime", s.c)
		if inodeSize != "128" {
			set(s.file, "crtime", s.cr)
			set(s.file, "atime_extra", s.aex)
			set(s.file, "mtime_extra", s.mex)
			set(s.file, "ctime_extra", s.cex)
			set(s.file, "crtime_extra", s.crex)
		}
	}
	cf := filepath.Join(t.TempDir(), "cmds")
	if err := os.WriteFile(cf, []byte(cmds), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(debugfs, "-w", "-f", cf, path).CombinedOutput(); err != nil {
		t.Fatalf("debugfs: %v\n%s", err, out)
	}
}

func gzipTo(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	zw, _ := gzip.NewWriterLevel(out, gzip.BestCompression)
	if _, err := io.Copy(zw, in); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	out.Close()
}

var times128Specs = []timesSpec{{
	file: "/full.txt",
	a:    "@1700000000", wantA: time.Unix(1700000000, 0),
	m: "@1715953530", wantM: time.Unix(1715953530, 0),
	c: "@1715953531", wantC: time.Unix(1715953531, 0),
	resSec: time.Second,
}}

func TestTimesRegenerateFixtures(t *testing.T) {
	if os.Getenv("EXT4_REGEN_TIMES") != "1" {
		t.Skip("set EXT4_REGEN_TIMES=1 to rebuild the times fixtures")
	}
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.img"), filepath.Join(dir, "b.img")
	buildTimesImage(t, a, "256", "ext4", times256Specs)
	gzipTo(t, a, times256Fixture)
	buildTimesImage(t, b, "128", "ext2", times128Specs)
	gzipTo(t, b, times128Fixture)
}

func openGz(t *testing.T, fixture []byte) filesystem.TimesReader {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(fixture))
	if err != nil {
		t.Fatal(err)
	}
	img := filepath.Join(t.TempDir(), "t.img")
	out, err := os.Create(img)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, zr); err != nil {
		t.Fatal(err)
	}
	out.Close()
	return openTimesImage(t, img)
}

func openTimesImage(t *testing.T, img string) filesystem.TimesReader {
	t.Helper()
	fs, err := Open(img, -1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fs.Close() })
	tr, ok := fs.(filesystem.TimesReader)
	if !ok {
		t.Fatal("ext4 is not a TimesReader")
	}
	return tr
}

func checkSpecs(t *testing.T, tr filesystem.TimesReader, specs []timesSpec, wantBtimeRes bool) {
	t.Helper()
	for _, s := range specs {
		ft, err := tr.Times(s.file)
		if err != nil {
			t.Fatalf("%s: %v", s.file, err)
		}
		t.Logf("%s: %+v", s.file, ft)
		for _, c := range []struct {
			n    string
			got  filesystem.Timestamp
			want time.Time
		}{{"atime", ft.Atime, s.wantA}, {"mtime", ft.Mtime, s.wantM}, {"ctime", ft.Ctime, s.wantC}} {
			g, ok := c.got.Get()
			if !ok || !g.Equal(c.want) {
				t.Errorf("%s %s = %v (%v), want %v", s.file, c.n, g, ok, c.want)
			}
			if c.got.Resolution() != s.resSec {
				t.Errorf("%s %s resolution %v, want %v", s.file, c.n, c.got.Resolution(), s.resSec)
			}
		}
		if s.wantB.IsZero() {
			if ft.Btime.Recorded() {
				t.Errorf("%s: btime %v, want unrecorded", s.file, ft.Btime)
			}
			continue
		}
		if g, ok := ft.Btime.Get(); !ok || !g.Equal(s.wantB) || ft.Btime.Resolution() != time.Nanosecond {
			t.Errorf("%s btime = %v (%v) res %v, want %v", s.file, g, ok, ft.Btime.Resolution(), s.wantB)
		}
	}
}

// TestTimesReferenceImages checks the decoder against inodes the e2fsprogs
// oracle wrote for known values (256-byte inodes, extra-epoch dates, a
// truncated extra area, a missing crtime).
func TestTimesReferenceImages(t *testing.T) {
	checkSpecs(t, openGz(t, times256Gz), times256Specs, true)
}

// TestTimesReferenceImageSmallInodes: 128-byte inodes (ext2) have no _extra
// words and no crtime.
func TestTimesReferenceImageSmallInodes(t *testing.T) {
	checkSpecs(t, openGz(t, times128Gz), times128Specs, false)
}

// TestTimesLiveReferenceTools rebuilds both images with the tools on this host,
// so a new e2fsprogs that encodes differently fails here.
func TestTimesLiveReferenceTools(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.img")
	buildTimesImage(t, a, "256", "ext4", times256Specs)
	checkSpecs(t, openTimesImage(t, a), times256Specs, true)
	b := filepath.Join(dir, "b.img")
	buildTimesImage(t, b, "128", "ext2", times128Specs)
	checkSpecs(t, openTimesImage(t, b), times128Specs, false)
}

func TestTimesRootSymlinkAndMissing(t *testing.T) {
	tr := openGz(t, times256Gz)
	if ft, err := tr.Times("/"); err != nil || !ft.Mtime.Recorded() {
		t.Fatalf("root: %+v %v", ft, err)
	}
	// "link" -> full.txt: the link's own inode, not the target's.
	lk, err := tr.Times("/link")
	if err != nil {
		t.Fatal(err)
	}
	full, _ := tr.Times("/full.txt")
	if lk.Mtime == full.Mtime {
		t.Errorf("Times followed the symlink: %v", lk.Mtime)
	}
	if _, err := tr.Times("/nope"); !errors.Is(err, iofs.ErrNotExist) {
		t.Fatalf("err = %v", err)
	}
	if _, err := tr.Times("/full.txt/x"); err == nil {
		t.Fatal("a file is not a directory")
	}
}

// rawInode builds a 256-byte inode image with the given extra_isize and fields.
func rawInode(size, extraIsize int, base, extra [3]uint32, cr, crex uint32) []byte {
	b := make([]byte, size)
	le := binary.LittleEndian
	put := func(off int, v uint32) {
		if off+4 <= size {
			le.PutUint32(b[off:], v)
		}
	}
	if size >= inodeOffExtraIsize+2 {
		le.PutUint16(b[inodeOffExtraIsize:], uint16(extraIsize))
	}
	for i, off := range []int{inodeOffATime, inodeOffMTime, inodeOffCTime} {
		put(off, base[i])
	}
	for i, off := range []int{inodeOffATimeExtra, inodeOffMTimeExtra, inodeOffCTimeExtra} {
		put(off, extra[i])
	}
	put(inodeOffCrtime, cr)
	put(inodeOffCrtimeExtra, crex)
	return b
}

// TestDecodeInodeTimes covers the encoding exactly with hand-built inodes,
// following ext4.h's table of epoch/msb combinations.
func TestDecodeInodeTimes(t *testing.T) {
	// Pre-1970: base word 0xFFFFFFFF = -1, extra epoch 0.
	ft := decodeInodeTimes(rawInode(256, 32, [3]uint32{0xFFFFFFFF, 0x80000000, 0}, [3]uint32{0, 0, 0}, 0, 0))
	if a, _ := ft.Atime.Get(); !a.Equal(time.Unix(-1, 0)) {
		t.Errorf("atime -1: %v", a)
	}
	if m, _ := ft.Mtime.Get(); !m.Equal(time.Unix(-0x80000000, 0)) {
		t.Errorf("mtime min: %v", m)
	}
	if c, ok := ft.Ctime.Get(); !ok || !c.Equal(time.Unix(0, 0)) {
		t.Errorf("1970 epoch is a time, not absence: %v %v", c, ok)
	}
	// ext4.h table: base 0x80000000 (msb set, read signed = -2^31) with epoch 1
	// is 0x080000000 + 2^32... = 2038-01-19; with epoch 3 and base 0x7FFFFFFF it
	// is 0x37FFFFFFF (year 2446).
	ft = decodeInodeTimes(rawInode(256, 32, [3]uint32{0x80000000, 0x7FFFFFFF, 0xFFFFFFFF}, [3]uint32{1, 3, 2}, 0, 0))
	for _, c := range []struct {
		got  filesystem.Timestamp
		want int64
	}{{ft.Atime, 0x80000000}, {ft.Mtime, 0x37FFFFFFF}, {ft.Ctime, 0x1FFFFFFFF}} {
		if g, _ := c.got.Get(); g.Unix() != c.want {
			t.Errorf("sec = %#x, want %#x", g.Unix(), c.want)
		}
	}
	// Nanoseconds: 999999999 << 2 is the largest valid word; 10^9 << 2 is not.
	ft = decodeInodeTimes(rawInode(256, 32, [3]uint32{1, 1, 1}, [3]uint32{999999999 << 2, 1000000000 << 2, 0}, 5, 999999999<<2))
	if a, _ := ft.Atime.Get(); a.Nanosecond() != 999999999 {
		t.Errorf("atime ns %d", a.Nanosecond())
	}
	if ft.Mtime.Recorded() {
		t.Error("nsec >= 1e9 must be unrecorded")
	}
	if b, ok := ft.Btime.Get(); !ok || b.Unix() != 5 || b.Nanosecond() != 999999999 {
		t.Errorf("btime %v", b)
	}
	// i_extra_isize 20 reaches the end of i_crtime (148) but not
	// i_crtime_extra (152): crtime in whole seconds... and the three _extra
	// words (ending at 144) fit.
	ft = decodeInodeTimes(rawInode(256, 20, [3]uint32{1, 1, 1}, [3]uint32{4, 4, 4}, 77, 0xFFFFFFFF))
	if b, ok := ft.Btime.Get(); !ok || b.Unix() != 77 || b.Nanosecond() != 0 {
		t.Errorf("crtime without crtime_extra: %v", b)
	}
	if ft.Atime.Resolution() != time.Nanosecond {
		t.Error("extra words fit at isize 20")
	}
	// extra_isize larger than the buffer is clamped to it.
	ft = decodeInodeTimes(rawInode(136, 0xFFFF, [3]uint32{1, 1, 1}, [3]uint32{4, 4, 4}, 0, 0))
	// A 136-byte buffer holds i_ctime_extra (132..136) but not i_atime_extra.
	if ft.Ctime.Resolution() != time.Nanosecond || ft.Atime.Resolution() != time.Second || ft.Btime.Recorded() {
		t.Errorf("clamp: %+v", ft)
	}
	// No extra area at all.
	ft = decodeInodeTimes(rawInode(128, 0, [3]uint32{9, 9, 9}, [3]uint32{}, 0, 0))
	if ft.Mtime.Resolution() != time.Second || ft.Btime.Recorded() {
		t.Errorf("128-byte inode: %+v", ft)
	}
	// Truncated buffer: nothing, no panic.
	if decodeInodeTimes(make([]byte, 100)) != (filesystem.FileTimes{}) {
		t.Error("short raw inode")
	}
	// All-zero crtime on a large inode: absent. Non-zero extra alone: present.
	if decodeInodeTimes(rawInode(256, 32, [3]uint32{}, [3]uint32{}, 0, 0)).Btime.Recorded() {
		t.Error("zero crtime")
	}
	if !decodeInodeTimes(rawInode(256, 32, [3]uint32{}, [3]uint32{}, 0, 4)).Btime.Recorded() {
		t.Error("crtime 0 with nsec is a time")
	}
}

func FuzzDecodeInodeTimes(f *testing.F) {
	f.Add(make([]byte, 256))
	f.Add(rawInode(256, 32, [3]uint32{1, 2, 3}, [3]uint32{4, 5, 6}, 7, 8))
	f.Add(rawInode(256, 0xFFFF, [3]uint32{0xFFFFFFFF, 0x80000000, 0x7FFFFFFF}, [3]uint32{0xFFFFFFFF, 3, 1000000000 << 2}, 0xFFFFFFFF, 0xFFFFFFFF))
	f.Add(rawInode(128, 0, [3]uint32{1, 2, 3}, [3]uint32{}, 0, 0))
	f.Fuzz(func(t *testing.T, raw []byte) {
		ft := decodeInodeTimes(raw)
		for _, ts := range []filesystem.Timestamp{ft.Atime, ft.Mtime, ft.Ctime, ft.Btime} {
			if g, ok := ts.Get(); ok {
				if g.IsZero() || (ts.Resolution() != time.Second && ts.Resolution() != time.Nanosecond) {
					t.Fatalf("recorded %v res %v", g, ts.Resolution())
				}
				if g.Unix() < -0x80000000 || g.Unix() > 0x37FFFFFFF {
					t.Fatalf("outside the encodable range: %v", g)
				}
			}
		}
	})
}
