// SPDX-License-Identifier: MIT

//go:build darwin

package scan

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// The fixture's home is a temp dir: its .Trash is the Trash.
func setTestTrash(t *testing.T, f *fixture, o *RemoveOptions) {}

func checkTrashNames(t *testing.T, wheres []string, name string) {
	t.Helper()
	want := []string{name, trashName(name, 2, false, 255)}
	for i, w := range wheres {
		if filepath.Base(w) != want[i] || filepath.Base(filepath.Dir(w)) != ".Trash" {
			t.Errorf("trashed as %q, want .Trash/%q", w, want[i])
		}
	}
	st, err := os.Lstat(filepath.Dir(wheres[0]))
	if err != nil || st.Mode().Perm() != 0o700 || !st.IsDir() {
		t.Errorf(".Trash: %v %v", st.Mode(), err)
	}
}

// A .Trash that is a symlink (or not owned by the user) is refused, and the
// target stays where it is.
func TestTrashDarwinSymlinkedTrash(t *testing.T) {
	f := newFixture(t)
	if err := os.Symlink(f.outside, filepath.Join(f.home, ".Trash")); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(f.root, "f")
	writeFile(t, file, 10)
	s, _, b := f.scan(t)
	tg, err := s.ResolveFile(b, 0, "f")
	if err != nil {
		t.Fatal(err)
	}
	_, err = Remove(context.Background(), tg, RemoveOptions{Mode: ModeTrash, Home: f.home})
	wantCode(t, err, CodeTrashUnavailable)
	if !exists(file) || exists(filepath.Join(f.outside, "f")) {
		t.Fatal("moved through a symlinked Trash")
	}
}

// Opt-in: one file into the real user's Trash, then removed from it.
func TestRealTrashDarwin(t *testing.T) {
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
	home, _ := os.UserHomeDir()
	if filepath.Dir(res.Where) != filepath.Join(home, ".Trash") {
		t.Fatalf("trashed to %q", res.Where)
	}
	t.Logf("trashed to %s", res.Where)
}
