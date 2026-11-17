// SPDX-License-Identifier: MIT

package scan

import (
	"path/filepath"
	"strings"
)

// RootGuard restricts paths to a directory subtree, resolving symlinks and
// ".." so that traversal and symlink escapes are rejected.
//
// Containment is proven from an OPEN HANDLE: the candidate directory is
// opened once, its identity is taken from that handle, and containment is
// established from the same handle (Unix: walking ".." with openat from the
// handle until the guard root's identity is met; Windows: the handle's final
// path). A path swapped for a symlink after the open cannot change the
// verdict, and the returned identity is what later opens are verified against.
type RootGuard struct {
	root  string
	id    Identity // identity of the root, taken from an open handle
	canon string   // Windows: canonical final path of the root handle
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
	g := &RootGuard{root: real}
	if err := g.initRoot(real); err != nil {
		return nil, err
	}
	return g, nil
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
// identity of the directory it named, taken from the same open handle that
// proved containment, so the caller can open resolved with ListSubdirsExpect /
// Options.ExpectRoot and detect a swap between check and use. A nil guard
// returns the absolute path and the identity of the opened directory (zero if
// it cannot be opened, which leaves verification off as before).
func (g *RootGuard) WithinResolved(p string) (resolved string, id Identity, ok bool) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", Identity{}, false
	}
	if g == nil {
		id, _ := rootID(abs)
		return abs, id, true
	}
	resolved, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return "", Identity{}, false
	}
	id, ok = g.contained(resolved)
	if !ok {
		return "", Identity{}, false
	}
	return resolved, id, true
}

// pathWithin reports whether p equals root or lies beneath it, comparing
// case-insensitively on a separator boundary.
func pathWithin(root, p string) bool {
	root = strings.TrimRight(root, `\/`)
	if len(p) < len(root) || !strings.EqualFold(p[:len(root)], root) {
		return false
	}
	if len(p) == len(root) {
		return true
	}
	return p[len(root)] == '\\' || p[len(root)] == '/'
}
