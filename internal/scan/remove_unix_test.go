// SPDX-License-Identifier: MIT

//go:build darwin || linux

package scan

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func linkDir(target, link string) error { return os.Symlink(target, link) }

// A symlink file target is removed as the link; a regular file target that
// became a symlink is refused.
func TestRemoveSymlinkFile(t *testing.T) {
	for _, m := range bothModes {
		t.Run(m.name, func(t *testing.T) {
			f := newFixture(t)
			link := filepath.Join(f.root, "link")
			if err := os.Symlink(f.secret, link); err != nil {
				t.Fatal(err)
			}
			plain := filepath.Join(f.root, "plain")
			writeFile(t, plain, 10)
			s, _, b := f.scan(t)
			tg, err := s.ResolveFile(b, 0, "link")
			if err != nil {
				t.Fatal(err)
			}
			res, err := Remove(context.Background(), tg, trashOpts(t, f, m.mode))
			if err != nil || !res.OK || exists(link) {
				t.Fatalf("res=%+v err=%v", res, err)
			}
			f.checkOutside(t)

			tg, err = s.ResolveFile(b, 0, "plain")
			if err != nil {
				t.Fatal(err)
			}
			os.Remove(plain)
			os.Symlink(f.secret, plain)
			_, err = Remove(context.Background(), tg, trashOpts(t, f, m.mode))
			wantCode(t, err, CodeChanged)
			f.checkOutside(t)
			if !exists(plain) {
				t.Fatal("the swapped-in link was removed")
			}
		})
	}
}

// A symlink standing where a protected folder is named (merged-/usr /lib64)
// is refused like the folder.
func TestRemoveProtectedSymlink(t *testing.T) {
	f := newFixture(t)
	link := filepath.Join(f.root, "lib64")
	if err := os.Symlink(f.outside, link); err != nil {
		t.Fatal(err)
	}
	testProtected = []string{link}
	defer func() { testProtected = nil }()
	s, _, b := f.scan(t)
	if _, err := s.ResolveFile(b, 0, "lib64"); ErrorCode(err) != CodeProtected {
		t.Fatalf("err=%v, want protected", err)
	}
}

// A file with another link outside the target is removed but frees nothing.
func TestRemoveHardlinkAccounting(t *testing.T) {
	f := newFixture(t)
	tdir := filepath.Join(f.root, "t")
	mkdirs(t, tdir)
	writeFile(t, filepath.Join(tdir, "own"), 100000)
	if err := os.Link(f.secret, filepath.Join(tdir, "shared")); err != nil {
		t.Fatal(err)
	}
	s, ids, b := f.scan(t)
	tg := resolveDir(t, s, b, ids, tdir)
	res, err := Remove(context.Background(), tg, RemoveOptions{Mode: ModePermanent})
	if err != nil || res.Files != 2 || res.FreedApparent >= 100000+5000 || res.FreedApparent < 100000 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	f.checkOutside(t)
}

func TestWalkUp(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b")
	mkdirs(t, deep)
	id := func(p string) Identity {
		i, err := rootID(p)
		if err != nil {
			t.Fatal(err)
		}
		return i
	}
	fd, err := openDir(deep)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	a, r, o := id(filepath.Join(root, "a")), id(root), id(t.TempDir())
	cases := []struct {
		stops []Identity
		want  bool
	}{
		{[]Identity{r}, true},
		{[]Identity{a, r}, true},
		{[]Identity{id(deep), a, r, r}, true},
		{[]Identity{r, a}, false}, // out of order
		{[]Identity{o}, false},
		{[]Identity{a, o}, false},
	}
	for i, c := range cases {
		if got := walkUp(fd, c.stops...); got != c.want {
			t.Errorf("case %d: %v want %v", i, got, c.want)
		}
	}
	if _, err := fdIdentity(fd); err != nil {
		t.Fatal("walkUp closed the caller's fd")
	}
}
