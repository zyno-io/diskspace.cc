// SPDX-License-Identifier: MIT

//go:build linux

package scan

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The fixture's home (opts.Home) holds the Trash; this process's
// XDG_DATA_HOME must then be ignored.
func setTestTrash(t *testing.T, f *fixture, o *RemoveOptions) {
	t.Setenv("XDG_DATA_HOME", filepath.Join(f.home, "not-this-one"))
}

func checkTrashNames(t *testing.T, wheres []string, name string) {
	t.Helper()
	want := []string{name, trashName(name, 2, false, 255)}
	for i, w := range wheres {
		dir := filepath.Dir(w)
		if filepath.Base(w) != want[i] || filepath.Base(dir) != "files" {
			t.Errorf("trashed as %q, want files/%q", w, want[i])
		}
		info := filepath.Join(filepath.Dir(dir), "info", want[i]+infoSuffix)
		if b, err := os.ReadFile(info); err != nil || !strings.HasPrefix(string(b), "[Trash Info]\nPath=/") {
			t.Errorf("info %s: %q %v", info, b, err)
		}
	}
}

func TestTrashInfoPath(t *testing.T) {
	if got := trashInfoPath("/a b/%é/x-y_z.~"); got != "/a%20b/%25%C3%A9/x-y_z.~" {
		t.Fatal(got)
	}
}

// The .trashinfo records the percent-encoded original path and the date.
// XDG_DATA_HOME places the Trash only when no other home is given.
func TestTrashLinuxInfo(t *testing.T) {
	t.Run("home", func(t *testing.T) { testTrashLinuxInfo(t, false) })
	t.Run("xdg", func(t *testing.T) { testTrashLinuxInfo(t, true) })
}

func testTrashLinuxInfo(t *testing.T, xdg bool) {
	f := newFixture(t)
	name := "a b%é.txt"
	writeFile(t, filepath.Join(f.root, name), 10)
	s, _, b := f.scan(t)
	tg, err := s.ResolveFile(b, 0, name)
	if err != nil {
		t.Fatal(err)
	}
	opts := trashOpts(t, f, ModeTrash)
	trash := filepath.Join(f.home, ".local", "share", "Trash")
	if xdg {
		t.Setenv("HOME", f.home)
		t.Setenv("XDG_DATA_HOME", filepath.Join(f.home, "data"))
		opts.Home = ""
		trash = filepath.Join(f.home, "data", "Trash")
	}
	res, err := Remove(context.Background(), tg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(f.home, "not-this-one")) {
		t.Fatal("used this process's XDG_DATA_HOME for another home")
	}
	if res.Where != filepath.Join(trash, "files", name) {
		t.Fatalf("where=%q", res.Where)
	}
	raw, err := os.ReadFile(filepath.Join(trash, "info", name+infoSuffix))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	real, _ := filepath.EvalSymlinks(f.root)
	if len(lines) != 3 || lines[0] != "[Trash Info]" || lines[1] != "Path="+trashInfoPath(filepath.Join(real, name)) {
		t.Fatalf("info:\n%s", raw)
	}
	when, err := time.ParseInLocation("2006-01-02T15:04:05", strings.TrimPrefix(lines[2], "DeletionDate="), time.Local)
	if err != nil || time.Since(when) > time.Minute {
		t.Fatalf("date %q: %v", lines[2], err)
	}
	for _, d := range []string{trash, filepath.Join(trash, "files"), filepath.Join(trash, "info")} {
		if st, err := os.Lstat(d); err != nil || st.Mode().Perm() != 0o700 {
			t.Errorf("%s: %v", d, err)
		}
	}
}

// A symlinked Trash is refused and nothing moves.
func TestTrashLinuxSymlinkedTrash(t *testing.T) {
	f := newFixture(t)
	mkdirs(t, filepath.Join(f.home, ".local", "share"))
	if err := os.Symlink(f.outside, filepath.Join(f.home, ".local", "share", "Trash")); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(f.root, "f")
	writeFile(t, file, 10)
	s, _, b := f.scan(t)
	tg, err := s.ResolveFile(b, 0, "f")
	if err != nil {
		t.Fatal(err)
	}
	_, err = Remove(context.Background(), tg, trashOpts(t, f, ModeTrash))
	wantCode(t, err, CodeTrashUnavailable)
	if !exists(file) {
		t.Fatal("moved")
	}
}

// Opt-in: one file into the real user's Trash, then removed from it.
func TestRealTrashLinux(t *testing.T) {
	if os.Getenv("DISKSPACE_REAL_TRASH") != "1" {
		t.Skip("set DISKSPACE_REAL_TRASH=1 to move a temp file into the real Trash")
	}
	f := newFixture(t)
	name := "diskspace-trash-test-" + strconv.FormatInt(time.Now().UnixNano(), 36)
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
	defer os.Remove(res.Where)
	defer os.Remove(filepath.Join(filepath.Dir(filepath.Dir(res.Where)), "info", filepath.Base(res.Where)+infoSuffix))
	t.Logf("trashed to %s", res.Where)
}
