// SPDX-License-Identifier: MIT

//go:build windows

package scan

import (
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

// linkDir creates a junction (no privilege needed, unlike a symlink).
func linkDir(target, link string) error {
	out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		return &os.PathError{Op: "mklink " + strings.TrimSpace(string(out)), Path: link, Err: err}
	}
	return nil
}

// The Recycle Bin is a temp dir next to the fixture.
func setTestTrash(t *testing.T, f *fixture, o *RemoveOptions) {
	dir := filepath.Join(f.parent, "bin")
	mkdirs(t, dir)
	o.hooks = &removeHooks{recycleDir: dir}
}

type recycleMeta struct {
	version, size uint64
	when          time.Time
	path          string
}

// parseRecycleInfo decodes a version 2 $I file.
func parseRecycleInfo(t *testing.T, b []byte) recycleMeta {
	t.Helper()
	if len(b) < 28 {
		t.Fatalf("$I too short: %d", len(b))
	}
	ft := binary.LittleEndian.Uint64(b[16:])
	n := int(binary.LittleEndian.Uint32(b[24:]))
	if len(b) != 28+2*n {
		t.Fatalf("$I length %d for %d chars", len(b), n)
	}
	u := make([]uint16, n)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[28+2*i:])
	}
	if u[n-1] != 0 {
		t.Fatal("$I path not NUL-terminated")
	}
	f := windows.Filetime{LowDateTime: uint32(ft), HighDateTime: uint32(ft >> 32)}
	return recycleMeta{
		version: binary.LittleEndian.Uint64(b),
		size:    binary.LittleEndian.Uint64(b[8:]),
		when:    time.Unix(0, f.Nanoseconds()),
		path:    string(utf16.Decode(u[:n-1])),
	}
}

func TestRecycleInfoRoundTrip(t *testing.T) {
	now := time.Now()
	p := `C:\Users\x\Fotos 😀\été.jpg`
	m := parseRecycleInfo(t, recycleInfo(p, 123456789, now))
	if m.version != 2 || m.size != 123456789 || m.path != p || m.when.Sub(now).Abs() > time.Microsecond {
		t.Fatalf("%+v", m)
	}
}

var recycleName = regexp.MustCompile(`^\$R([A-Z0-9]{6})(\.txt)?$`)

func checkTrashNames(t *testing.T, wheres []string, name string) {
	t.Helper()
	for _, w := range wheres {
		m := recycleName.FindStringSubmatch(filepath.Base(w))
		if m == nil || m[2] != filepath.Ext(name) {
			t.Fatalf("recycled as %q", w)
		}
		b, err := os.ReadFile(filepath.Join(filepath.Dir(w), "$I"+m[1]+m[2]))
		if err != nil {
			t.Fatal(err)
		}
		meta := parseRecycleInfo(t, b)
		if filepath.Base(meta.path) != name || !filepath.IsAbs(meta.path) || strings.HasPrefix(meta.path, `\\?\`) {
			t.Errorf("$I path %q", meta.path)
		}
		if st, _ := os.Stat(w); st == nil || meta.size != uint64(st.Size()) {
			t.Errorf("$I size %d", meta.size)
		}
	}
}

// A read-only file is removed permanently and recycled.
func TestRemoveReadOnly(t *testing.T) {
	for _, m := range bothModes {
		t.Run(m.name, func(t *testing.T) {
			f := newFixture(t)
			file := filepath.Join(f.root, "ro.txt")
			writeFile(t, file, 10)
			if err := os.Chmod(file, 0o444); err != nil {
				t.Fatal(err)
			}
			s, _, b := f.scan(t)
			tg, err := s.ResolveFile(b, 0, "ro.txt")
			if err != nil {
				t.Fatal(err)
			}
			res, err := Remove(context.Background(), tg, trashOpts(t, f, m.mode))
			if err != nil || !res.OK || exists(file) {
				t.Fatalf("res=%+v err=%v", res, err)
			}
		})
	}
}

// A junction inside a recycled directory moves with it, its target untouched;
// a directory's $I has no extension and records TrashSize.
func TestRecycleDirWithJunction(t *testing.T) {
	f := newFixture(t)
	tdir := filepath.Join(f.root, "t.d")
	mkdirs(t, tdir)
	writeFile(t, filepath.Join(tdir, "x"), 10)
	if err := linkDir(f.outside, filepath.Join(tdir, "j")); err != nil {
		t.Fatal(err)
	}
	s, ids, b := f.scan(t)
	tg := resolveDir(t, s, b, ids, tdir)
	o := trashOpts(t, f, ModeTrash)
	o.TrashSize = 4242
	res, err := Remove(context.Background(), tg, o)
	if err != nil || exists(tdir) {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	m := recycleName.FindStringSubmatch(filepath.Base(res.Where))
	if m == nil || m[2] != "" {
		t.Fatalf("recycled as %q", res.Where)
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(res.Where), "$I"+m[1]))
	if err != nil {
		t.Fatal(err)
	}
	if meta := parseRecycleInfo(t, raw); meta.size != 4242 || filepath.Base(meta.path) != "t.d" {
		t.Fatalf("%+v", meta)
	}
	if !exists(filepath.Join(res.Where, "x")) || !exists(filepath.Join(res.Where, "j", "SECRET")) {
		t.Fatal("contents not moved")
	}
	f.checkOutside(t)
}

// Opt-in: one file into the real Recycle Bin, then removed from it.
func TestRealRecycleBin(t *testing.T) {
	if os.Getenv("DISKSPACE_REAL_TRASH") != "1" {
		t.Skip("set DISKSPACE_REAL_TRASH=1 to move a temp file into the real Recycle Bin")
	}
	f := newFixture(t)
	name := "diskspace-trash-test-" + strconv.FormatInt(time.Now().UnixNano(), 36) + ".txt"
	writeFile(t, filepath.Join(f.root, name), 1234)
	s, _, b := f.scan(t)
	tg, err := s.ResolveFile(b, 0, name)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Remove(context.Background(), tg, RemoveOptions{Mode: ModeTrash})
	if err != nil {
		t.Fatal(err)
	}
	m := recycleName.FindStringSubmatch(filepath.Base(res.Where))
	info := filepath.Join(filepath.Dir(res.Where), "$I"+m[1]+m[2])
	defer os.Remove(info)
	defer os.Remove(res.Where)
	if !strings.Contains(res.Where, `\$Recycle.Bin\S-`) || !exists(info) {
		t.Fatalf("recycled to %q", res.Where)
	}
	t.Logf("recycled to %s", res.Where)
	// What Explorer shows: the Recycle Bin shell folder lists it by its
	// original name and location.
	out, err := exec.Command("powershell", "-NoProfile", "-Command",
		`(New-Object -ComObject Shell.Application).NameSpace(10).Items() | % { $_.ExtendedProperty('System.Recycle.DeletedFrom') + '\' + $_.Name }`).CombinedOutput()
	if err != nil || !strings.Contains(string(out), strings.TrimSuffix(name, ".txt")) {
		t.Fatalf("not listed by the shell (%v):\n%s", err, out)
	}
}
