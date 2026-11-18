// SPDX-License-Identifier: MIT

package scan

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zyno-io/diskspace.cc/internal/proto"
)

// ---- fixtures ----

func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func writeFile(t *testing.T, p string, n int) {
	t.Helper()
	if err := os.WriteFile(p, make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// scanTree scans root and returns the scanner and the announced id of every
// directory by path.
func scanTree(t *testing.T, root string) (*Scanner, map[string]uint64) {
	t.Helper()
	s, err := New(Options{Root: root, Threads: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Run(context.Background(), func(proto.Record) {}); err != nil {
		t.Fatal(err)
	}
	ids := map[string]uint64{root: 0}
	for id := uint64(1); id < s.nextID.Load(); id++ {
		if p, ok := s.PathOf(id); ok {
			ids[p] = id
		}
	}
	return s, ids
}

// fixture is a scan root inside a temp parent, with a sibling "outside" dir
// holding a file that must survive every test.
type fixture struct {
	parent, root, outside, secret string
	home                          string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	parent := t.TempDir()
	f := &fixture{
		parent:  parent,
		root:    filepath.Join(parent, "root"),
		outside: filepath.Join(parent, "outside"),
		home:    filepath.Join(parent, "home"),
	}
	mkdirs(t, f.root, f.outside, f.home)
	f.secret = filepath.Join(f.outside, "SECRET")
	writeFile(t, f.secret, 5000)
	return f
}

func (f *fixture) checkOutside(t *testing.T) {
	t.Helper()
	if !exists(f.secret) {
		t.Fatal("a file outside the root was removed")
	}
}

func (f *fixture) scan(t *testing.T) (*Scanner, map[string]uint64, Bounds) {
	t.Helper()
	s, ids := scanTree(t, f.root)
	return s, ids, s.Bounds(nil, f.home)
}

func resolveDir(t *testing.T, s *Scanner, b Bounds, ids map[string]uint64, p string) Target {
	t.Helper()
	id, ok := ids[p]
	if !ok {
		t.Fatalf("%s was not announced", p)
	}
	tg, err := s.ResolveDir(b, id)
	if err != nil {
		t.Fatalf("ResolveDir(%s): %v", p, err)
	}
	return tg
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if got := ErrorCode(err); err == nil || got != code {
		t.Fatalf("err=%v (code %q), want code %q", err, got, code)
	}
}

var bothModes = []struct {
	name string
	mode RemoveMode
}{{"trash", ModeTrash}, {"permanent", ModePermanent}}

// trashOpts returns options that keep the Trash inside the test's temp dirs.
func trashOpts(t *testing.T, f *fixture, mode RemoveMode) RemoveOptions {
	o := RemoveOptions{Mode: mode, Home: f.home}
	setTestTrash(t, f, &o)
	return o
}

// ---- tests ----

func TestValidName(t *testing.T) {
	for _, n := range []string{"a", "a b", ".a", "..a", "...", "\xff", strings.Repeat("x", maxNameBytes)} {
		if !ValidName(n) {
			t.Errorf("ValidName(%q) = false", n)
		}
	}
	for _, n := range []string{"", ".", "..", "a/b", "/", "a\x00", strings.Repeat("x", maxNameBytes+1)} {
		if ValidName(n) {
			t.Errorf("ValidName(%q) = true", n)
		}
	}
	if runtime.GOOS == "windows" {
		for _, n := range []string{`a\b`, `\`, "a:b", "C:"} {
			if ValidName(n) {
				t.Errorf("ValidName(%q) = true", n)
			}
		}
	}
}

// A valid name always addresses exactly one entry directly inside its parent.
func FuzzValidName(f *testing.F) {
	for _, s := range []string{"", ".", "..", "a/b", `a\b`, "a\x00b", "C:", "a:b", "...", " ", "a/../b", "\xff\xfe", strings.Repeat("y", 4097)} {
		f.Add(s)
	}
	parent := filepath.Join(string(filepath.Separator)+"p", "q")
	f.Fuzz(func(t *testing.T, name string) {
		if !ValidName(name) {
			return
		}
		if name == "" || name == "." || name == ".." || len(name) > maxNameBytes || strings.ContainsAny(name, "/\x00") {
			t.Fatalf("accepted %q", name)
		}
		j := filepath.Join(parent, name)
		if filepath.Dir(j) != parent || filepath.Base(j) != name {
			t.Fatalf("%q escapes its parent: %q", name, j)
		}
	})
}

func TestTrashName(t *testing.T) {
	cases := []struct {
		name string
		n    int
		dir  bool
		want string
	}{
		{"a.txt", 1, false, "a.txt"},
		{"a.txt", 2, false, "a 2.txt"},
		{"a.tar.gz", 3, false, "a.tar 3.gz"},
		{"a.txt", 2, true, "a.txt 2"},
		{".bashrc", 2, false, ".bashrc 2"},
		{"noext", 10, false, "noext 10"},
	}
	for _, c := range cases {
		if got := trashName(c.name, c.n, c.dir, 255); got != c.want {
			t.Errorf("trashName(%q,%d,%v)=%q want %q", c.name, c.n, c.dir, got, c.want)
		}
	}
	long := strings.Repeat("é", 200) + ".txt" // 404 bytes
	for _, n := range []int{1, 2, 999} {
		got := trashName(long, n, false, 245)
		if len(got) > 245 || !strings.HasSuffix(got, ".txt") || !strings.HasPrefix(got, "é") {
			t.Errorf("long n=%d: %q (%d bytes)", n, got, len(got))
		}
		if !strings.Contains(got, "é") || strings.ContainsRune(got, '�') {
			t.Errorf("long n=%d: split a UTF-8 sequence", n)
		}
	}
}

func TestRemoveFilePermanent(t *testing.T) {
	f := newFixture(t)
	file := filepath.Join(f.root, "f")
	writeFile(t, file, 5000)
	writeFile(t, filepath.Join(f.root, "keep"), 10)
	s, _, b := f.scan(t)
	tg, err := s.ResolveFile(b, 0, "f")
	if err != nil {
		t.Fatal(err)
	}
	if tg.Dir || tg.Apparent != 5000 || tg.ID.IsZero() {
		t.Fatalf("target %+v", tg)
	}
	var live Counters
	res, err := Remove(context.Background(), tg, RemoveOptions{Mode: ModePermanent, Live: &live})
	if err != nil || !res.OK || res.Files != 1 || res.FreedApparent != 5000 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if live.Files.Load() != 1 {
		t.Errorf("live files=%d", live.Files.Load())
	}
	if exists(file) || !exists(filepath.Join(f.root, "keep")) {
		t.Fatal("wrong files removed")
	}
	// Removing it again: gone.
	_, err = Remove(context.Background(), tg, RemoveOptions{Mode: ModePermanent})
	wantCode(t, err, CodeNotFound)
	if _, err := s.ResolveFile(b, 0, "../outside"); ErrorCode(err) != CodeInvalidName {
		t.Fatalf("../outside: %v", err)
	}
}

// A tree with nested dirs, read-only files and a link to a directory outside
// the root: everything inside is removed, the link as a link.
func TestRemoveDirPermanent(t *testing.T) {
	f := newFixture(t)
	tdir := filepath.Join(f.root, "t")
	mkdirs(t, filepath.Join(tdir, "a", "b", "c"), filepath.Join(tdir, "e"))
	files := 0
	for _, d := range []string{tdir, filepath.Join(tdir, "a"), filepath.Join(tdir, "a", "b", "c"), filepath.Join(tdir, "e")} {
		for _, n := range []string{"x", "y"} {
			writeFile(t, filepath.Join(d, n), 3000)
			files++
		}
	}
	ro := filepath.Join(tdir, "a", "readonly")
	writeFile(t, ro, 100)
	files++
	if err := os.Chmod(ro, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := linkDir(f.outside, filepath.Join(tdir, "a", "link")); err != nil {
		t.Fatal(err)
	}
	s, ids, b := f.scan(t)
	tg := resolveDir(t, s, b, ids, tdir)
	res, err := Remove(context.Background(), tg, RemoveOptions{Mode: ModePermanent})
	if err != nil || !res.OK {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if exists(tdir) {
		t.Fatal("target still exists")
	}
	f.checkOutside(t)
	// files + the link; dirs t, a, b, c, e (+ the link on Windows, a directory reparse point)
	if res.Files+res.Dirs != uint64(files)+1+5 {
		t.Errorf("files=%d dirs=%d, want %d entries", res.Files, res.Dirs, files+6)
	}
	if res.FreedApparent < uint64(files-1)*3000 {
		t.Errorf("freedApparent=%d", res.FreedApparent)
	}
}

func TestRemoveTargetSwappedForLink(t *testing.T) {
	for _, m := range bothModes {
		t.Run(m.name, func(t *testing.T) {
			f := newFixture(t)
			tdir := filepath.Join(f.root, "t")
			mkdirs(t, tdir)
			writeFile(t, filepath.Join(tdir, "x"), 10)
			s, ids, b := f.scan(t)
			tg := resolveDir(t, s, b, ids, tdir)
			moved := filepath.Join(f.parent, "t.moved")
			if err := os.Rename(tdir, moved); err != nil {
				t.Fatal(err)
			}
			if err := linkDir(f.outside, tdir); err != nil {
				t.Fatal(err)
			}
			_, err := Remove(context.Background(), tg, trashOpts(t, f, m.mode))
			wantCode(t, err, CodeChanged)
			f.checkOutside(t)
			if !exists(filepath.Join(moved, "x")) || !exists(tdir) {
				t.Fatal("something was removed")
			}
		})
	}
}

func TestRemoveFileReplaced(t *testing.T) {
	for _, m := range bothModes {
		t.Run(m.name, func(t *testing.T) {
			f := newFixture(t)
			file := filepath.Join(f.root, "f")
			writeFile(t, file, 10)
			s, _, b := f.scan(t)
			tg, err := s.ResolveFile(b, 0, "f")
			if err != nil {
				t.Fatal(err)
			}
			other := filepath.Join(f.root, "other")
			writeFile(t, other, 20)
			if err := os.Rename(other, file); err != nil {
				t.Fatal(err)
			}
			_, err = Remove(context.Background(), tg, trashOpts(t, f, m.mode))
			wantCode(t, err, CodeChanged)
			if st, err := os.Stat(file); err != nil || st.Size() != 20 {
				t.Fatal("the replacement was removed")
			}
		})
	}
}

// An ancestor swapped for a link to a look-alike tree outside the root.
func TestRemoveAncestorSwap(t *testing.T) {
	for _, m := range bothModes {
		t.Run(m.name, func(t *testing.T) {
			f := newFixture(t)
			tdir := filepath.Join(f.root, "a", "b", "t")
			mkdirs(t, tdir, filepath.Join(f.outside, "b", "t"))
			look := filepath.Join(f.outside, "b", "t", "SECRET2")
			writeFile(t, look, 10)
			s, ids, b := f.scan(t)
			tg := resolveDir(t, s, b, ids, tdir)
			if err := os.Rename(filepath.Join(f.root, "a"), filepath.Join(f.parent, "a.moved")); err != nil {
				t.Fatal(err)
			}
			if err := linkDir(f.outside, filepath.Join(f.root, "a")); err != nil {
				t.Fatal(err)
			}
			_, err := Remove(context.Background(), tg, trashOpts(t, f, m.mode))
			wantCode(t, err, CodeChanged)
			f.checkOutside(t)
			if !exists(look) || !exists(filepath.Join(f.parent, "a.moved", "b", "t")) {
				t.Fatal("something was removed")
			}
		})
	}
}

// The target's ancestor moved out of the root (same directories, same
// identities) and a link put in its place: containment fails.
func TestRemoveMovedOutsideRoot(t *testing.T) {
	for _, m := range bothModes {
		t.Run(m.name, func(t *testing.T) {
			f := newFixture(t)
			tdir := filepath.Join(f.root, "a", "b", "t")
			mkdirs(t, tdir)
			writeFile(t, filepath.Join(tdir, "x"), 10)
			s, ids, b := f.scan(t)
			tg := resolveDir(t, s, b, ids, tdir)
			out := filepath.Join(f.parent, "outA")
			if err := os.Rename(filepath.Join(f.root, "a"), out); err != nil {
				t.Fatal(err)
			}
			if err := linkDir(out, filepath.Join(f.root, "a")); err != nil {
				t.Fatal(err)
			}
			_, err := Remove(context.Background(), tg, trashOpts(t, f, m.mode))
			wantCode(t, err, CodeOutsideRoot)
			if !exists(filepath.Join(out, "b", "t", "x")) {
				t.Fatal("the moved target was removed")
			}
		})
	}
}

// A directory inside the target is swapped between being listed and opened.
func TestRemoveInnerSwapMidRecursion(t *testing.T) {
	for _, variant := range []string{"link", "dir"} {
		t.Run(variant, func(t *testing.T) {
			f := newFixture(t)
			tdir := filepath.Join(f.root, "t")
			inner := filepath.Join(tdir, "inner")
			mkdirs(t, inner, filepath.Join(tdir, "x"))
			writeFile(t, filepath.Join(inner, "f"), 10)
			writeFile(t, filepath.Join(tdir, "x", "f2"), 10)
			s, ids, b := f.scan(t)
			tg := resolveDir(t, s, b, ids, tdir)
			moved := filepath.Join(f.parent, "inner.moved")
			opts := RemoveOptions{Mode: ModePermanent, hooks: &removeHooks{beforeOpen: func(p string) {
				if p != inner {
					return
				}
				if err := os.Rename(inner, moved); err != nil {
					t.Error(err)
				}
				var err error
				if variant == "link" {
					err = linkDir(f.outside, inner)
				} else {
					err = os.Rename(f.outside, inner)
				}
				if err != nil {
					t.Error(err)
				}
			}}}
			res, err := Remove(context.Background(), tg, opts)
			wantCode(t, err, CodeChanged)
			if !res.Partial || exists(filepath.Join(tdir, "x")) {
				t.Fatalf("res=%+v", res)
			}
			if !exists(filepath.Join(moved, "f")) {
				t.Fatal("the moved directory was entered")
			}
			if variant == "link" {
				f.checkOutside(t)
			} else if !exists(filepath.Join(inner, "SECRET")) {
				t.Fatal("the swapped-in directory was entered")
			}
		})
	}
}

func TestRemoveOtherDeviceSkipped(t *testing.T) {
	f := newFixture(t)
	tdir := filepath.Join(f.root, "t")
	mnt := filepath.Join(tdir, "sub", "mnt")
	mkdirs(t, mnt, filepath.Join(tdir, "other"))
	writeFile(t, filepath.Join(mnt, "f"), 10)
	writeFile(t, filepath.Join(tdir, "other", "g"), 10)
	s, ids, b := f.scan(t)
	tg := resolveDir(t, s, b, ids, tdir)
	opts := RemoveOptions{Mode: ModePermanent, hooks: &removeHooks{devOf: func(p string, dev uint64) uint64 {
		if p == mnt {
			return dev + 1
		}
		return dev
	}}}
	res, err := Remove(context.Background(), tg, opts)
	wantCode(t, err, CodeMountPoint)
	if !res.Partial || res.SkippedCount != 1 || len(res.Skipped) != 1 || res.Skipped[0] != mnt || res.ErrorCount != 0 {
		t.Fatalf("res=%+v", res)
	}
	if !exists(filepath.Join(mnt, "f")) || exists(filepath.Join(tdir, "other")) {
		t.Fatal("wrong entries removed")
	}
}

func TestRemoveProtected(t *testing.T) {
	f := newFixture(t)
	prot := filepath.Join(f.root, "a", "prot")
	mkdirs(t, filepath.Join(prot, "c"))
	testProtected = []string{prot}
	defer func() { testProtected = nil }()
	s, ids, b := f.scan(t)
	for _, p := range []string{filepath.Join(f.root, "a"), prot} {
		if _, err := s.ResolveDir(b, ids[p]); ErrorCode(err) != CodeProtected {
			t.Errorf("%s: %v, want protected", p, err)
		}
	}
	if _, err := s.ResolveDir(b, ids[filepath.Join(prot, "c")]); err != nil {
		t.Errorf("contents of a protected folder: %v", err)
	}
	if _, err := s.ResolveDir(b, 0); ErrorCode(err) != CodeProtected {
		t.Errorf("scan root: %v", err)
	}
	// By identity, whatever the spelling, and for ancestors.
	pr := b.protection()
	for _, p := range []string{prot, filepath.Join(f.root, "a")} {
		id, ok := pathIdentity(p)
		if !ok || !pr.covers(filepath.Join(f.parent, "elsewhere"), id) {
			t.Errorf("identity of %s not protected", p)
		}
	}
	if id, _ := pathIdentity(filepath.Join(prot, "c")); pr.covers(filepath.Join(f.parent, "elsewhere"), id) {
		t.Error("content of a protected folder covered by identity")
	}
	// A flagged directory is refused.
	id := ids[filepath.Join(prot, "c")]
	e, _ := s.arena.get(id)
	s.arena.set(id, e.parent-1, e.name, e.ident, proto.ChildOtherDevice)
	if _, err := s.ResolveDir(b, id); ErrorCode(err) != CodeMountPoint {
		t.Errorf("flagged: %v", err)
	}
	s.arena.set(id, e.parent-1, e.name, e.ident, proto.ChildDupDir)
	if _, err := s.ResolveDir(b, id); ErrorCode(err) != CodeNotScanned {
		t.Errorf("dup: %v", err)
	}
}

// The standard protected folders, by path.
func TestProtectedPaths(t *testing.T) {
	home := t.TempDir()
	p := newProtection(home)
	sep := string(filepath.Separator)
	yes := []string{home, filepath.Join(home, "Documents"), filepath.Join(home, "Downloads"), filepath.Dir(home)}
	no := []string{filepath.Join(home, "Documents", "x"), filepath.Join(home, "Downloads", "big.iso"), filepath.Join(home, "proj")}
	switch runtime.GOOS {
	case "darwin":
		yes = append(yes, "/", "/Users", "/System", "/Applications", "/Library", "/usr", "/private/var", "/Volumes",
			"/System/Volumes/Data", "/System/Volumes/Data/Users", filepath.Join(home, "Library"), filepath.Join(home, ".Trash"))
		no = append(no, "/Users/x/Library/Caches", filepath.Join(home, "Library", "Caches"), "/Applications/Foo.app")
	case "linux":
		yes = append(yes, "/", "/home", "/usr", "/etc", "/var", filepath.Join(home, ".local", "share", "Trash"))
		no = append(no, filepath.Join(home, ".cache"), "/usr/share/doc/foo")
	case "windows":
		d := os.Getenv("SystemDrive")
		if d == "" {
			d = "C:"
		}
		yes = append(yes, d+sep, d+`\Windows`, d+`\Users`, d+`\ProgramData`, d+`\$Recycle.Bin`, filepath.Join(home, "AppData"))
		no = append(no, filepath.Join(home, "AppData", "Local", "Temp"), d+`\Windows\Temp\x`)
	}
	for _, q := range yes {
		if !p.covers(q, Identity{}) {
			t.Errorf("%s not protected", q)
		}
	}
	for _, q := range no {
		if p.covers(q, Identity{}) {
			t.Errorf("%s protected", q)
		}
	}
}

func TestRemoveCanceled(t *testing.T) {
	f := newFixture(t)
	tdir := filepath.Join(f.root, "t")
	mkdirs(t, tdir)
	s, ids, b := f.scan(t)
	tg := resolveDir(t, s, b, ids, tdir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Remove(ctx, tg, RemoveOptions{Mode: ModePermanent})
	wantCode(t, err, CodeCanceled)
	if !errors.Is(err, context.Canceled) || !exists(tdir) {
		t.Fatal(err)
	}
	// Mid-removal: stops between entries.
	mkdirs(t, filepath.Join(tdir, "d1"), filepath.Join(tdir, "d2"))
	for i := 0; i < 20; i++ {
		writeFile(t, filepath.Join(tdir, "d1", strings.Repeat("f", i+1)), 1)
		writeFile(t, filepath.Join(tdir, "d2", strings.Repeat("f", i+1)), 1)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	opts := RemoveOptions{Mode: ModePermanent, hooks: &removeHooks{beforeOpen: func(p string) {
		if filepath.Dir(p) == tdir {
			cancel() // when the first subdirectory is opened
		}
	}}}
	res, err := Remove(ctx, tg, opts)
	if ErrorCode(err) != CodeCanceled || res.OK || !exists(tdir) || res.Files >= 40 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

// Trash: a file twice under the same name (collision naming) and a directory.
func TestTrashCollision(t *testing.T) {
	f := newFixture(t)
	file := filepath.Join(f.root, "f.txt")
	tdir := filepath.Join(f.root, "t")
	mkdirs(t, tdir)
	writeFile(t, filepath.Join(tdir, "x"), 10)
	var wheres []string
	for i := 0; i < 2; i++ {
		writeFile(t, file, 1000+i)
		s, _, b := f.scan(t)
		tg, err := s.ResolveFile(b, 0, "f.txt")
		if err != nil {
			t.Fatal(err)
		}
		res, err := Remove(context.Background(), tg, trashOpts(t, f, ModeTrash))
		if err != nil || !res.OK || res.Files != 1 || res.Where == "" {
			t.Fatalf("res=%+v err=%v", res, err)
		}
		if exists(file) {
			t.Fatal("file still in place")
		}
		st, err := os.Lstat(res.Where)
		if err != nil || st.Size() != int64(1000+i) {
			t.Fatalf("trashed file at %q: %v", res.Where, err)
		}
		wheres = append(wheres, res.Where)
	}
	if wheres[0] == wheres[1] {
		t.Fatal("collision overwrote")
	}
	checkTrashNames(t, wheres, "f.txt")
	s, ids, b := f.scan(t)
	tg := resolveDir(t, s, b, ids, tdir)
	res, err := Remove(context.Background(), tg, trashOpts(t, f, ModeTrash))
	if err != nil || !res.OK || res.Dirs != 1 || exists(tdir) || !exists(filepath.Join(res.Where, "x")) {
		t.Fatalf("dir: res=%+v err=%v", res, err)
	}
	f.checkOutside(t)
}
