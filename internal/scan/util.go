// SPDX-License-Identifier: MIT

package scan

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/zyno-io/diskspace.cc/internal/proto"
)

const sep = string(os.PathSeparator)

// joinPath joins a cleaned directory path and an entry name.
func joinPath(parent, name string) string {
	if strings.HasSuffix(parent, sep) {
		return parent + name
	}
	return parent + sep + name
}

// fkey identifies a file or directory on a device.
type fkey struct{ dev, ino uint64 }

// shardedSet is a 64-way sharded set of fkeys.
type shardedSet struct {
	shards [64]struct {
		mu sync.Mutex
		m  map[fkey]struct{}
		_  [40]byte
	}
}

// add inserts k and reports whether it was newly added.
func (s *shardedSet) add(k fkey) bool {
	h := (k.ino ^ k.dev*0x9E3779B97F4A7C15) * 0xC2B2AE3D27D4EB4F
	sh := &s.shards[(h>>56)&63]
	sh.mu.Lock()
	if sh.m == nil {
		sh.m = make(map[fkey]struct{}, 256)
	}
	_, dup := sh.m[k]
	if !dup {
		sh.m[k] = struct{}{}
	}
	sh.mu.Unlock()
	return !dup
}

// ---- arena of (parent, name) for announced dirs ----

const (
	arenaBits = 14
	arenaSize = 1 << arenaBits
	arenaMask = arenaSize - 1
)

type arenaEnt struct {
	name   string
	parent uint64 // parent id + 1; 0 = unset
}

type arena struct {
	chunks []atomic.Pointer[[arenaSize]arenaEnt]
}

func newArena(maxDirs uint64) *arena {
	return &arena{chunks: make([]atomic.Pointer[[arenaSize]arenaEnt], (maxDirs>>arenaBits)+1)}
}

func (a *arena) chunk(id uint64) *[arenaSize]arenaEnt {
	p := &a.chunks[id>>arenaBits]
	if c := p.Load(); c != nil {
		return c
	}
	nc := new([arenaSize]arenaEnt)
	if p.CompareAndSwap(nil, nc) {
		return nc
	}
	return p.Load()
}

func (a *arena) set(id, parent uint64, name string) {
	if id>>arenaBits >= uint64(len(a.chunks)) {
		return
	}
	a.chunk(id)[id&arenaMask] = arenaEnt{name: name, parent: parent + 1}
}

func (a *arena) get(id uint64) (arenaEnt, bool) {
	if id>>arenaBits >= uint64(len(a.chunks)) {
		return arenaEnt{}, false
	}
	c := a.chunks[id>>arenaBits].Load()
	if c == nil {
		return arenaEnt{}, false
	}
	e := c[id&arenaMask]
	return e, e.parent != 0
}

func (a *arena) path(root string, id uint64, maxDepth int) (string, bool) {
	if id == 0 {
		return root, true
	}
	var parts []string
	for n := 0; id != 0; n++ {
		if n > maxDepth+2 {
			return "", false
		}
		e, ok := a.get(id)
		if !ok {
			return "", false
		}
		parts = append(parts, e.name)
		id = e.parent - 1
	}
	elems := make([]string, 0, len(parts)+1)
	elems = append(elems, root)
	for i := len(parts) - 1; i >= 0; i-- {
		elems = append(elems, parts[i])
	}
	return filepath.Join(elems...), true
}

// ---- top-K min-heap of files ----

// topHeap keeps the K largest files (min-heap on Alloc, ties by name).
type topHeap []proto.FileEntry

func topLess(a, b *proto.FileEntry) bool {
	if a.Alloc != b.Alloc {
		return a.Alloc < b.Alloc
	}
	return a.Name > b.Name
}

func (h *topHeap) offer(k int, f proto.FileEntry) {
	s := *h
	if len(s) < k {
		s = append(s, f)
		i := len(s) - 1
		for i > 0 {
			p := (i - 1) / 2
			if !topLess(&s[i], &s[p]) {
				break
			}
			s[i], s[p] = s[p], s[i]
			i = p
		}
		*h = s
		return
	}
	if k == 0 || !topLess(&s[0], &f) {
		return
	}
	s[0] = f
	i := 0
	for {
		l, r, m := 2*i+1, 2*i+2, i
		if l < len(s) && topLess(&s[l], &s[m]) {
			m = l
		}
		if r < len(s) && topLess(&s[r], &s[m]) {
			m = r
		}
		if m == i {
			break
		}
		s[i], s[m] = s[m], s[i]
		i = m
	}
}
