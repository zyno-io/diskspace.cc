// SPDX-License-Identifier: MIT

package scan

import (
	"sync"
	"sync/atomic"
)

// foldGroup tracks outstanding folded jobs under one announced target.
type foldGroup struct {
	target  uint64
	pending atomic.Int64
}

type job struct {
	path  string
	id    uint64 // announced id (unused for folded jobs)
	depth int32
	grp   *foldGroup // non-nil: folded job
	want  Identity   // expected identity of the directory at path
}

// deque: owner pushes/pops at the tail (LIFO), thieves steal from the head.
type deque struct {
	mu    sync.Mutex
	items []job
	head  int
}

func (d *deque) push(j job) {
	d.mu.Lock()
	d.items = append(d.items, j)
	d.mu.Unlock()
}

func (d *deque) pushBatch(js []job) {
	d.mu.Lock()
	d.items = append(d.items, js...)
	d.mu.Unlock()
}

func (d *deque) pop() (job, bool) {
	d.mu.Lock()
	n := len(d.items)
	if n == d.head {
		d.mu.Unlock()
		return job{}, false
	}
	j := d.items[n-1]
	d.items[n-1] = job{}
	d.items = d.items[:n-1]
	if len(d.items) == d.head {
		d.items = d.items[:0]
		d.head = 0
	}
	d.mu.Unlock()
	return j, true
}

func (d *deque) steal() (job, bool) {
	d.mu.Lock()
	if len(d.items) == d.head {
		d.mu.Unlock()
		return job{}, false
	}
	j := d.items[d.head]
	d.items[d.head] = job{}
	d.head++
	if d.head == len(d.items) {
		d.items = d.items[:0]
		d.head = 0
	} else if d.head > 1024 && d.head*2 > len(d.items) {
		n := copy(d.items, d.items[d.head:])
		clear(d.items[n:])
		d.items = d.items[:n]
		d.head = 0
	}
	d.mu.Unlock()
	return j, true
}

func (w *worker) trySteal() (job, bool) {
	ws := w.s.workers
	n := len(ws)
	w.rng ^= w.rng << 13
	w.rng ^= w.rng >> 17
	w.rng ^= w.rng << 5
	start := int(w.rng % uint32(n))
	for i := 0; i < n; i++ {
		v := ws[(start+i)%n]
		if v == w {
			continue
		}
		if j, ok := v.dq.steal(); ok {
			return j, true
		}
	}
	return job{}, false
}

// next returns the next job, parking when idle. false = terminate.
func (w *worker) next() (job, bool) {
	s := w.s
	for {
		if s.cancelled.Load() || s.pending.Load() == 0 {
			return job{}, false
		}
		if j, ok := w.dq.pop(); ok {
			return j, true
		}
		if j, ok := w.trySteal(); ok {
			return j, true
		}
		s.mu.Lock()
		s.idle.Add(1)
		if j, ok := w.trySteal(); ok {
			s.idle.Add(-1)
			s.mu.Unlock()
			return j, true
		}
		if s.cancelled.Load() || s.pending.Load() == 0 {
			s.idle.Add(-1)
			s.mu.Unlock()
			return job{}, false
		}
		s.cond.Wait()
		s.idle.Add(-1)
		s.mu.Unlock()
	}
}

// wake signals up to n parked workers.
func (s *Scanner) wake(n int) {
	idle := int(s.idle.Load())
	if idle == 0 {
		return
	}
	n = min(n, idle)
	s.mu.Lock()
	for i := 0; i < n; i++ {
		s.cond.Signal()
	}
	s.mu.Unlock()
}

func (w *worker) loop() {
	s := w.s
	for {
		j, ok := w.next()
		if !ok {
			return
		}
		w.process(j)
		if s.pending.Add(-1) == 0 {
			s.mu.Lock()
			s.cond.Broadcast()
			s.mu.Unlock()
		}
	}
}
