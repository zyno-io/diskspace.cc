// SPDX-License-Identifier: MIT

// Package scan implements the parallel, work-stealing disk scanner.
package scan

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/zyno-io/diskspace.cc/internal/proto"
)

// Options configures a scan.
type Options struct {
	Root         string // absolute, cleaned
	CrossMounts  bool
	TopFiles     int    // default 8 (negative disables)
	MinFileBytes uint64 // default 1<<20
	Threads      int    // default clamp(2*NumCPU, 8, 32)
	MaxDirs      uint64 // default 2_000_000 (announced dirs)
	MaxDepth     int    // default 512
}

// Stats is a snapshot of scan counters.
type Stats struct {
	Dirs, Files, Alloc, Apparent, Denied, Errors, Vanished uint64
	SkippedMounts, HardlinksDeduped, Folded                uint64
	CurrentPath                                            string
}

// wstats are per-worker counters (single writer, atomic readers).
type wstats struct {
	dirs, files, alloc, apparent, denied, errors atomic.Uint64
	vanished, skipped, hardlinks, folded         atomic.Uint64
	_                                            [48]byte
}

// Scanner scans one tree. A Scanner can be Run once.
type Scanner struct {
	opts             Options
	plat             platState
	rootDev, rootIno uint64

	nextID  atomic.Uint64
	arena   *arena
	visited shardedSet
	links   shardedSet

	workers []*worker
	pending atomic.Int64
	started atomic.Bool

	cancelled atomic.Bool
	mu        sync.Mutex
	cond      *sync.Cond
	idle      atomic.Int32

	out     chan proto.Record
	curPath atomic.Pointer[string]
}

// hookBeforeOpen is a test hook invoked before each directory is opened.
var hookBeforeOpen func(path string)

// New validates the root and applies defaults.
func New(opts Options) (*Scanner, error) {
	if opts.Root == "" {
		return nil, errors.New("scan: empty root")
	}
	root := filepath.Clean(opts.Root)
	fi, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		r, err := filepath.EvalSymlinks(root)
		if err != nil {
			return nil, err
		}
		root = r
		if fi, err = os.Stat(root); err != nil {
			return nil, err
		}
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("scan: %s is not a directory", root)
	}
	opts.Root = root
	if opts.TopFiles == 0 {
		opts.TopFiles = 8
	} else if opts.TopFiles < 0 {
		opts.TopFiles = 0
	}
	if opts.MinFileBytes == 0 {
		opts.MinFileBytes = 1 << 20
	}
	if opts.Threads <= 0 {
		opts.Threads = min(max(2*runtime.NumCPU(), 8), 32)
	}
	if opts.MaxDirs == 0 {
		opts.MaxDirs = 2_000_000
	}
	if opts.MaxDepth <= 0 {
		opts.MaxDepth = 512
	}
	s := &Scanner{opts: opts}
	s.cond = sync.NewCond(&s.mu)
	dev, ino, err := rootID(root)
	if err != nil {
		return nil, err
	}
	s.rootDev, s.rootIno = dev, ino
	s.plat.init(root, dev, opts.CrossMounts)
	s.visited.add(fkey{dev, ino})
	s.nextID.Store(1)
	s.arena = newArena(opts.MaxDirs)
	s.workers = make([]*worker, opts.Threads)
	for i := range s.workers {
		s.workers[i] = &worker{s: s, id: i, rng: uint32(i)*2654435761 + 1}
	}
	return s, nil
}

// Progress returns a snapshot of the counters; safe during Run.
func (s *Scanner) Progress() Stats {
	var st Stats
	for _, w := range s.workers {
		st.Dirs += w.st.dirs.Load()
		st.Files += w.st.files.Load()
		st.Alloc += w.st.alloc.Load()
		st.Apparent += w.st.apparent.Load()
		st.Denied += w.st.denied.Load()
		st.Errors += w.st.errors.Load()
		st.Vanished += w.st.vanished.Load()
		st.SkippedMounts += w.st.skipped.Load()
		st.HardlinksDeduped += w.st.hardlinks.Load()
		st.Folded += w.st.folded.Load()
	}
	if p := s.curPath.Load(); p != nil {
		st.CurrentPath = *p
	}
	return st
}

// PathOf reconstructs the path of an announced directory id.
func (s *Scanner) PathOf(id uint64) (string, bool) {
	return s.arena.path(s.opts.Root, id, s.opts.MaxDepth)
}

// Run scans the tree. emit is called from the calling goroutine only, in
// parent-before-child order. Run returns after all workers have drained.
func (s *Scanner) Run(ctx context.Context, emit func(proto.Record)) (Stats, error) {
	if !s.started.CompareAndSwap(false, true) {
		return s.Progress(), errors.New("scan: Run called twice")
	}
	if err := ctx.Err(); err != nil {
		return s.Progress(), err
	}
	s.out = make(chan proto.Record, 8192)
	s.pending.Store(1)
	s.workers[0].dq.push(job{path: s.opts.Root, id: 0, depth: 0})

	stop := context.AfterFunc(ctx, s.cancel)
	defer stop()

	var live atomic.Int32
	live.Store(int32(len(s.workers)))
	for _, w := range s.workers {
		go func() {
			w.loop()
			if live.Add(-1) == 0 {
				close(s.out)
			}
		}()
	}
	dropped := false
	for rec := range s.out {
		if dropped || s.cancelled.Load() || ctx.Err() != nil {
			dropped = true // keep draining so workers never block, but stop emitting
			continue
		}
		emit(rec)
	}
	st := s.Progress()
	if dropped || s.pending.Load() != 0 {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		return st, context.Canceled
	}
	return st, nil
}

func (s *Scanner) cancel() {
	s.cancelled.Store(true)
	s.mu.Lock()
	s.cond.Broadcast()
	s.mu.Unlock()
}
