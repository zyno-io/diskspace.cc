// SPDX-License-Identifier: MIT

package scan

import (
	"errors"
	"slices"
	"strings"

	"github.com/zyno-io/diskspace.cc/internal/proto"
)

type kid struct {
	name  string
	path  string
	flags uint64
	ident Identity
}

type worker struct {
	s     *Scanner
	id    int
	dq    deque
	rng   uint32
	sc    readScratch
	kids  []kid
	top   topHeap
	batch []job
	st    wstats
}

func (w *worker) emit(r proto.Record) { w.s.out <- r }

// childChunkBytes bounds the encoded subdirectory list carried by one
// DirRecord or DirMoreRecord (a chunk may overshoot by at most one child,
// <= proto.MaxNameLen+4 bytes). Larger lists continue in DirMoreRecords, so a
// single record is always far below the 1 MiB message limit. A var so tests
// can shrink it.
var childChunkBytes = 256 << 10

// childCost is the encoded size of one announced child: name length varint +
// (possibly truncated) name + flags varint, rounded up.
func childCost(k *kid) int {
	return min(len(k.name), proto.MaxNameLen) + 4
}

// reserveIDs allocates n consecutive ids unless that would exceed MaxDirs.
func (s *Scanner) reserveIDs(n uint64) (uint64, bool) {
	for {
		cur := s.nextID.Load()
		if cur+n > s.opts.MaxDirs {
			return 0, false
		}
		if s.nextID.CompareAndSwap(cur, cur+n) {
			return cur, true
		}
	}
}

func (w *worker) process(j job) {
	s := w.s
	opts := &s.opts
	if hookBeforeOpen != nil {
		hookBeforeOpen(j.path)
	}
	w.kids = w.kids[:0]
	w.top = w.top[:0]
	var (
		ownAlloc, ownApp, ownFiles, entryErrs, vanishedEntries uint64
		deduped, skipped                                       uint64
		dflags                                                 uint64
		ticks                                                  int
	)
	topK, minBytes := opts.TopFiles, opts.MinFileBytes
	folded := j.grp != nil

	self, err := readDirEntries(j.path, j.want, &w.sc, func(e *rawEntry) bool {
		ticks++
		if ticks&4095 == 0 && s.cancelled.Load() {
			return false
		}
		if e.Err != nil {
			if isGone(e.Err) {
				vanishedEntries++
			} else {
				entryErrs++
			}
			return true
		}
		if e.Kind == kindDir {
			cp := joinPath(j.path, e.Name)
			var cf uint64
			switch {
			case e.ChildFlags != 0:
				cf = e.ChildFlags
			case s.plat.excluded(j.path, e.Name):
				cf = proto.ChildExcluded
			case !s.plat.allowedDev(e.Dev):
				cf = proto.ChildOtherDevice
				skipped++
			case s.plat.bindMount(cp):
				cf = proto.ChildBindMount
				skipped++
			case !s.visited.add(fkey{e.Dev, e.Ino}):
				cf = proto.ChildDupDir
			}
			w.kids = append(w.kids, kid{name: e.Name, path: cp, flags: cf, ident: e.ID})
			return true
		}
		if e.Dedup && !s.links.add(fkey{e.Dev, e.Ino}) {
			deduped++
			return true
		}
		ownFiles++
		ownAlloc += e.Alloc
		ownApp += e.Apparent
		if topK > 0 && e.Alloc >= minBytes {
			fl := e.Flags
			if badName(e.Name) {
				fl |= proto.FileBadName
			}
			w.top.offer(topK, proto.FileEntry{Name: e.Name, Alloc: e.Alloc, Apparent: e.Apparent, Flags: fl})
		}
		return true
	})
	ownAlloc += self.Alloc
	ownApp += self.Apparent
	st := &w.st
	if err != nil {
		switch {
		case isDenied(err):
			dflags |= proto.DirReadDenied
			st.denied.Add(1)
		case isGone(err), errors.Is(err, ErrChanged):
			// A directory that is no longer the one announced is treated
			// as vanished and never read.
			dflags |= proto.DirVanished
			st.vanished.Add(1)
		default:
			dflags |= proto.DirReadError
			st.errors.Add(1)
		}
	}
	if entryErrs > 0 {
		dflags |= proto.DirHasEntryErrors
		st.errors.Add(entryErrs)
	}
	if vanishedEntries > 0 {
		st.vanished.Add(vanishedEntries)
	}
	if deduped > 0 {
		st.hardlinks.Add(deduped)
	}
	if skipped > 0 {
		st.skipped.Add(skipped)
	}
	st.files.Add(ownFiles)
	st.alloc.Add(ownAlloc)
	st.apparent.Add(ownApp)
	if st.dirs.Add(1)&31 == 1 {
		p := j.path
		s.curPath.Store(&p)
	}

	nDesc := 0
	for i := range w.kids {
		if w.kids[i].flags == 0 {
			nDesc++
		}
	}

	if folded {
		st.folded.Add(1)
		w.emit(&proto.FoldRecord{TargetID: j.grp.target, Alloc: ownAlloc, Apparent: ownApp, Files: ownFiles, Dirs: 1})
		if nDesc > 0 && !s.cancelled.Load() {
			j.grp.pending.Add(int64(nDesc))
			w.spawn(j, 0, j.grp)
		}
		if j.grp.pending.Add(-1) == 0 {
			w.emit(&proto.FoldEndRecord{TargetID: j.grp.target})
		}
		return
	}

	rec := &proto.DirRecord{
		ID:          j.id,
		Flags:       dflags,
		OwnAlloc:    ownAlloc,
		OwnApparent: ownApp,
		OwnFiles:    ownFiles,
		EntryErrors: entryErrs,
	}
	var grp *foldGroup
	var first uint64
	var more []*proto.DirMoreRecord
	announce := false
	if n := len(w.kids); n > 0 {
		fold := false
		if nDesc > 0 && int(j.depth)+1 > opts.MaxDepth {
			fold = true
		} else if f, ok := s.reserveIDs(uint64(n)); ok {
			first, announce = f, true
		} else if nDesc > 0 {
			fold = true
		}
		if fold {
			rec.Flags |= proto.DirFoldedChildren
			grp = &foldGroup{target: j.id}
			grp.pending.Store(int64(nDesc))
		} else if announce {
			rec.FirstChildID = first
			rec.TotalDirs = uint64(n)
			for i := range w.kids {
				k := &w.kids[i]
				s.arena.set(first+uint64(i), j.id, k.name, k.ident, k.flags)
			}
			// Split the child list into chunks of ~childChunkBytes: the first
			// rides in the DirRecord, the rest in DirMoreRecords.
			cut := chunkEnd(w.kids, 0)
			rec.Dirs = childDirs(w.kids[:cut])
			for start := cut; start < n; start = cut {
				cut = chunkEnd(w.kids, start)
				more = append(more, &proto.DirMoreRecord{ID: j.id, FirstChildID: first + uint64(start), Dirs: childDirs(w.kids[start:cut])})
			}
		}
	}
	if len(w.top) > 0 {
		slices.SortFunc(w.top, func(a, b proto.FileEntry) int {
			switch {
			case a.Alloc > b.Alloc:
				return -1
			case a.Alloc < b.Alloc:
				return 1
			}
			return strings.Compare(a.Name, b.Name)
		})
		rec.Files = slices.Clone(w.top)
	}
	w.emit(rec) // parent record (and its continuations) precede any child job
	for _, m := range more {
		w.emit(m)
	}
	if nDesc > 0 && !s.cancelled.Load() {
		w.spawn(j, first, grp)
	}
}

// spawn pushes child jobs for unflagged kids. grp != nil makes them folded.
func (w *worker) spawn(j job, first uint64, grp *foldGroup) {
	s := w.s
	w.batch = w.batch[:0]
	for i := range w.kids {
		k := &w.kids[i]
		if k.flags != 0 {
			continue
		}
		jb := job{path: k.path, depth: j.depth + 1, grp: grp, want: k.ident}
		if grp == nil {
			jb.id = first + uint64(i)
		}
		w.batch = append(w.batch, jb)
	}
	n := len(w.batch)
	s.pending.Add(int64(n))
	w.dq.pushBatch(w.batch)
	s.wake(n)
	clear(w.batch)
}

// chunkEnd returns the end index of the chunk of kids starting at start: it
// holds at least one child and stops once childChunkBytes is reached.
func chunkEnd(kids []kid, start int) int {
	size, i := 0, start
	for i < len(kids) {
		c := childCost(&kids[i])
		if i > start && size+c > childChunkBytes {
			break
		}
		size += c
		i++
	}
	return i
}

func childDirs(kids []kid) []proto.ChildDir {
	out := make([]proto.ChildDir, len(kids))
	for i := range kids {
		out[i] = proto.ChildDir{Name: kids[i].name, Flags: kids[i].flags}
	}
	return out
}
