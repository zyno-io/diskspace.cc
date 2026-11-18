// SPDX-License-Identifier: MIT

package scan

import (
	"os"
	"path/filepath"
)

// Protected folders can't be removed as a whole (their contents can): the
// filesystem and system roots, the homes root, every user home, the standard
// folders directly inside a home, volume roots, and any ancestor of one of
// those. A directory is protected when its identity is that of a protected
// folder or of an ancestor of one (robust against case, symlinked spellings
// and macOS firmlinks), or when its canonical path names one or lies above
// one (covers folders that could not be stat'ed, and mount points, which are
// never stat'ed so a dead network mount can't hang a removal).

// testProtected adds protected folders in tests.
var testProtected []string

type protection struct {
	paths []string              // protected folders (as given and symlink-resolved), plus mount points
	ids   map[Identity]struct{} // identities of protected folders and their ancestors
}

func newProtection(home string) *protection {
	p := &protection{ids: map[Identity]struct{}{}}
	stated := map[string]bool{}
	addID := func(q string) {
		for !stated[q] {
			stated[q] = true
			if id, ok := pathIdentity(q); ok {
				p.ids[id] = struct{}{}
			}
			up := filepath.Dir(q)
			if up == q {
				return
			}
			q = up
		}
	}
	list := append(protectedPaths(home), testProtected...)
	for _, q := range list {
		q = filepath.Clean(q)
		p.paths = append(p.paths, q)
		real, err := filepath.EvalSymlinks(q)
		if err != nil {
			continue
		}
		if real != q {
			p.paths = append(p.paths, real)
		}
		addID(real)
	}
	p.paths = append(p.paths, mountRoots()...)
	return p
}

// covers reports whether removing the directory with canonical path canon and
// identity id would remove a protected folder.
func (p *protection) covers(canon string, id Identity) bool {
	if _, ok := p.ids[id]; ok {
		return true
	}
	for _, c := range canonForms(canon) {
		for _, q := range p.paths {
			if pathWithin(c, q) {
				return true
			}
		}
	}
	return false
}

// userHomes returns home, the current user's home, and every directory in
// the homes roots, without duplicates.
func userHomes(home string, roots []string, extra ...string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(h string) {
		if h == "" || !filepath.IsAbs(h) {
			return
		}
		h = filepath.Clean(h)
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	add(home)
	if h, err := os.UserHomeDir(); err == nil {
		add(h)
	}
	for _, h := range extra {
		add(h)
	}
	for _, r := range roots {
		ents, err := os.ReadDir(r)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if e.IsDir() || e.Type()&os.ModeSymlink != 0 {
				add(filepath.Join(r, e.Name()))
			}
		}
	}
	return out
}

// withHomeFolders appends each home and its standard folders to out.
func withHomeFolders(out, homes, folders []string) []string {
	for _, h := range homes {
		out = append(out, h)
		for _, f := range folders {
			out = append(out, filepath.Join(h, f))
		}
	}
	return out
}
