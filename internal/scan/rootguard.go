// SPDX-License-Identifier: MIT

package scan

import (
	"os"
	"path/filepath"
)

// RootGuard restricts paths to a directory subtree, resolving symlinks and
// ".." so that traversal and symlink escapes are rejected.
type RootGuard struct {
	root string
	fi   os.FileInfo
}

// NewRootGuard builds a guard for root. The root must exist.
func NewRootGuard(root string) (*RootGuard, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(real)
	if err != nil {
		return nil, err
	}
	return &RootGuard{root: real, fi: fi}, nil
}

// Root returns the resolved root path.
func (g *RootGuard) Root() string {
	if g == nil {
		return ""
	}
	return g.root
}

// Within reports whether p (after resolving symlinks) is the root or lies
// beneath it. A nil guard permits everything.
func (g *RootGuard) Within(p string) bool {
	if g == nil {
		return true
	}
	_, _, ok := g.WithinResolved(p)
	return ok
}

// WithinResolved is Within that also returns the symlink-resolved path and the
// identity of the directory it named, so the caller can open resolved with
// ListSubdirsExpect / Options.ExpectRoot and detect a swap between check and
// use. A nil guard returns the absolute path and a zero (unchecked) identity.
func (g *RootGuard) WithinResolved(p string) (resolved string, id Identity, ok bool) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", Identity{}, false
	}
	if g == nil {
		return abs, Identity{}, true
	}
	cur, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", Identity{}, false
	}
	resolved = cur
	for {
		fi, err := os.Stat(cur)
		if err != nil {
			return "", Identity{}, false
		}
		if os.SameFile(g.fi, fi) {
			id, err = rootID(resolved)
			if err != nil {
				return "", Identity{}, false
			}
			return resolved, id, true
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", Identity{}, false
		}
		cur = parent
	}
}
