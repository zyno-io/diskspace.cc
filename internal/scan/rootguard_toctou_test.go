// SPDX-License-Identifier: MIT

//go:build darwin || linux

package scan

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/zyno-io/diskspace.cc/internal/proto"
)

const toctouAttempts = 2000

// startFlipper swaps name <-> name.alt (real dir <-> symlink) until stopped.
func startFlipper(t *testing.T, name string) (stop func()) {
	var halt atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !halt.Load() {
			if err := swapPaths(name, name+".alt"); err != nil {
				t.Errorf("swap: %v", err)
				return
			}
		}
	}()
	return func() { halt.Store(true); <-done }
}

// The guard checks the last path component while it flips to a symlink that
// leaves the root; New(ExpectRoot) must never accept the outside directory.
func TestRootGuardRaceScanStartLastComponent(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	outside := filepath.Join(parent, "outside")
	os.MkdirAll(filepath.Join(root, "a", "b"), 0o755)
	os.MkdirAll(outside, 0o755)
	os.WriteFile(filepath.Join(outside, "SECRET"), make([]byte, 5000), 0o600)
	os.Symlink(outside, filepath.Join(root, "a", "b.alt"))
	outID, err := rootID(outside)
	if err != nil {
		t.Fatal(err)
	}
	g, err := NewRootGuard(root)
	if err != nil {
		t.Fatal(err)
	}
	stop := startFlipper(t, filepath.Join(root, "a", "b"))
	defer stop()
	p := filepath.Join(root, "a", "b")
	escapes, accepted := 0, 0
	for i := 0; i < toctouAttempts; i++ {
		_, id, ok := g.WithinResolved(p)
		if !ok {
			continue
		}
		accepted++
		s, err := New(Options{Root: p, ExpectRoot: id, Threads: 2})
		if err != nil {
			continue
		}
		if s.rootIdent == outID {
			escapes++
			continue
		}
		var leaked bool
		s.Run(context.Background(), func(r proto.Record) {
			if d, ok := r.(*proto.DirRecord); ok && len(d.Files) > 0 {
				leaked = true
			}
		})
		if leaked {
			escapes++
		}
	}
	stop()
	t.Logf("attempts=%d accepted=%d escapes=%d", toctouAttempts, accepted, escapes)
	if escapes != 0 {
		t.Fatalf("scan accepted a directory outside --root %d times", escapes)
	}
}

// BROWSE flow with an ANCESTOR flipping.
func TestRootGuardRaceBrowseAncestor(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	outside := filepath.Join(parent, "outside")
	os.MkdirAll(filepath.Join(root, "a", "b"), 0o755)
	os.MkdirAll(filepath.Join(outside, "b", "SECRET-SUBDIR"), 0o755)
	os.Symlink(outside, filepath.Join(root, "a.alt"))
	g, err := NewRootGuard(root)
	if err != nil {
		t.Fatal(err)
	}
	stop := startFlipper(t, filepath.Join(root, "a"))
	defer stop()
	p := filepath.Join(root, "a", "b")
	leaks := 0
	for i := 0; i < toctouAttempts; i++ {
		resolved, id, ok := g.WithinResolved(p)
		if !ok {
			continue
		}
		if names, err := ListSubdirsExpect(resolved, id); err == nil && len(names) > 0 {
			leaks++
		}
	}
	stop()
	if leaks != 0 {
		t.Fatalf("browse listed a directory outside --root %d times", leaks)
	}
}

// SCAN_START flow with an ANCESTOR flipping (the junction variant on Windows).
func TestRootGuardRaceScanStartAncestor(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	outside := filepath.Join(parent, "outside")
	os.MkdirAll(filepath.Join(root, "a", "b"), 0o755)
	os.MkdirAll(filepath.Join(outside, "b"), 0o755)
	os.WriteFile(filepath.Join(outside, "b", "SECRET"), make([]byte, 5000), 0o600)
	os.Symlink(outside, filepath.Join(root, "a.alt"))
	outID, err := rootID(filepath.Join(outside, "b"))
	if err != nil {
		t.Fatal(err)
	}
	g, err := NewRootGuard(root)
	if err != nil {
		t.Fatal(err)
	}
	stop := startFlipper(t, filepath.Join(root, "a"))
	defer stop()
	p := filepath.Join(root, "a", "b")
	escapes := 0
	for i := 0; i < toctouAttempts; i++ {
		_, id, ok := g.WithinResolved(p)
		if !ok {
			continue
		}
		s, err := New(Options{Root: p, ExpectRoot: id, Threads: 2})
		if err != nil {
			continue
		}
		st, _ := s.Run(context.Background(), func(proto.Record) {})
		if s.rootIdent == outID || st.Files > 0 {
			escapes++
		}
	}
	stop()
	if escapes != 0 {
		t.Fatalf("scan read a directory outside --root %d times", escapes)
	}
}

// Containment is decided from the handle: a symlink pointing out is rejected,
// and a nested real dir is accepted with the identity of that directory.
func TestRootGuardHandleContainment(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	outside := filepath.Join(parent, "outside")
	os.MkdirAll(filepath.Join(root, "x", "y"), 0o755)
	os.MkdirAll(outside, 0o755)
	os.Symlink(outside, filepath.Join(root, "link"))
	g, err := NewRootGuard(root)
	if err != nil {
		t.Fatal(err)
	}
	if !g.Within(root) {
		t.Error("root not within itself")
	}
	if g.Within(outside) || g.Within(parent) || g.Within(filepath.Join(root, "link")) {
		t.Error("outside accepted")
	}
	want, _ := rootID(filepath.Join(root, "x", "y"))
	_, id, ok := g.WithinResolved(filepath.Join(root, "x", "y"))
	if !ok || id != want {
		t.Errorf("nested: ok=%v id=%v want=%v", ok, id, want)
	}
}
