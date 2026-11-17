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
	abs, err := filepath.Abs(p)
	if err != nil {
		return false
	}
	cur, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return false
	}
	for {
		fi, err := os.Stat(cur)
		if err != nil {
			return false
		}
		if os.SameFile(g.fi, fi) {
			return true
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return false
		}
		cur = parent
	}
}
