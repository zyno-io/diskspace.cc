// SPDX-License-Identifier: MIT

//go:build !windows

package scan

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// escapeFixture builds root/a/b and outside/b (holding a file), returning
// root, the outside dir, and the swap function that replaces root/a with a
// symlink to outside.
func escapeFixture(t *testing.T) (root, outside string, swap func()) {
	t.Helper()
	parent := t.TempDir()
	root = filepath.Join(parent, "root")
	outside = filepath.Join(parent, "outside")
	for _, d := range []string{filepath.Join(root, "a", "b"), filepath.Join(outside, "b")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "b", "SECRET"), make([]byte, 5000), 0o600); err != nil {
		t.Fatal(err)
	}
	swap = func() {
		if err := os.Rename(filepath.Join(root, "a"), filepath.Join(parent, "a.moved")); err != nil {
			t.Error(err)
			return
		}
		if err := os.Symlink(outside, filepath.Join(root, "a")); err != nil {
			t.Error(err)
		}
	}
	return
}

// A parent directory swapped for a symlink between announcing and opening a
// child must not make the scanner read outside the root.
func TestScanChildOpenAncestorSwap(t *testing.T) {
	root, _, swap := escapeFixture(t)
	target := filepath.Join(root, "a", "b")
	hookBeforeOpen = func(p string) {
		if p == target {
			swap()
		}
	}
	defer func() { hookBeforeOpen = nil }()
	r := runScan(t, Options{Root: root})
	hookBeforeOpen = nil
	if r.stats.Files != 0 {
		t.Fatalf("scanner read %d file(s) outside the root", r.stats.Files)
	}
	if r.stats.Vanished != 1 {
		t.Fatalf("Vanished=%d, want 1", r.stats.Vanished)
	}
}

// Every announced directory's recorded identity matches the directory on disk.
func TestDirIdentityRecorded(t *testing.T) {
	root := t.TempDir()
	genTree(t, root, 11, 60, 100)
	r := runScan(t, Options{Root: root})
	r.verify(t, r.s.opts)
	n := 0
	for _, rec := range r.dirRecs {
		p, ok := r.s.PathOf(rec.ID)
		if !ok {
			t.Fatalf("PathOf(%d)", rec.ID)
		}
		want, err := rootID(p)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := r.s.DirIdentity(rec.ID)
		if !ok || got != want {
			t.Fatalf("DirIdentity(%d)=%v,%v want %v", rec.ID, got, ok, want)
		}
		n++
	}
	if n < 10 {
		t.Fatalf("only %d records", n)
	}
	if _, ok := r.s.DirIdentity(1 << 40); ok {
		t.Fatal("DirIdentity bogus id ok")
	}
}

func TestListDirFilesExpect(t *testing.T) {
	root, outside, swap := escapeFixture(t)
	os.WriteFile(filepath.Join(root, "a", "b", "mine"), make([]byte, 100), 0o600)
	p := filepath.Join(root, "a", "b")
	want, err := rootID(p)
	if err != nil {
		t.Fatal(err)
	}
	res, err := ListDirFilesExpect(p, want, 10)
	if err != nil || len(res.Files) != 1 {
		t.Fatalf("matching identity: %v %+v", err, res)
	}
	swap()
	res, err = ListDirFilesExpect(p, want, 10)
	if !errors.Is(err, ErrChanged) || res != nil {
		t.Fatalf("after swap (%s): err=%v res=%+v", outside, err, res)
	}
}

func TestWithinResolvedBindsIdentity(t *testing.T) {
	root, _, swap := escapeFixture(t)
	g, err := NewRootGuard(root)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "a", "b")
	resolved, id, ok := g.WithinResolved(p)
	if !ok || id.IsZero() {
		t.Fatalf("WithinResolved: %q %v %v", resolved, id, ok)
	}
	if _, err := ListSubdirsExpect(resolved, id); err != nil {
		t.Fatal(err)
	}
	// Swap an ancestor after the check, before the open: resolved now leads
	// outside the root through the symlink.
	swap()
	if _, err := ListSubdirsExpect(resolved, id); !errors.Is(err, ErrChanged) {
		t.Fatalf("swapped ancestor not detected: %v", err)
	}
	if _, _, ok := g.WithinResolved(filepath.Join(root, "a", "b")); ok {
		t.Fatal("swapped path still within root")
	}
}

func TestNewExpectRootMismatch(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	id, err := rootID(a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{Root: a, ExpectRoot: id}); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{Root: b, ExpectRoot: id}); !errors.Is(err, ErrChanged) {
		t.Fatalf("err=%v, want ErrChanged", err)
	}
}
