// SPDX-License-Identifier: MIT

package scan

import (
	"context"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zyno-io/diskspace.cc/internal/proto"
)

// While paused no directory is opened; after Resume the totals equal an
// unpaused scan's.
func TestPauseResume(t *testing.T) {
	root := t.TempDir()
	genTree(t, root, 31, 600, 1200)
	ref := refWalk(t, root)

	s, err := New(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	var opens atomic.Int64
	hookBeforeOpen = func(string) { opens.Add(1) }
	defer func() { hookBeforeOpen = nil }()

	r := &result{s: s}
	var n atomic.Int64
	paused := make(chan struct{})
	var once atomic.Bool
	done := make(chan error, 1)
	go func() {
		var err error
		r.stats, err = s.Run(context.Background(), func(rec proto.Record) {
			r.recs = append(r.recs, rec)
			if n.Add(1) == 50 && once.CompareAndSwap(false, true) {
				s.Pause()
				close(paused)
			}
		})
		done <- err
	}()
	select {
	case <-paused:
	case <-time.After(10 * time.Second):
		t.Fatal("scan never reached the pause point")
	}
	time.Sleep(50 * time.Millisecond) // let in-flight directories finish
	at := opens.Load()
	time.Sleep(200 * time.Millisecond)
	if got := opens.Load(); got != at {
		t.Fatalf("%d directories opened while paused", got-at)
	}
	if !s.Paused() {
		t.Fatal("Paused() = false")
	}
	select {
	case err := <-done:
		t.Fatalf("Run returned while paused: %v", err)
	default:
	}
	s.Resume()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("scan did not finish after Resume")
	}
	hookBeforeOpen = nil
	r.verify(t, r.s.opts)
	r.compare(t, ref)
}

// Cancelling a paused scan returns promptly with context.Canceled and leaves
// no goroutines behind.
func TestCancelWhilePaused(t *testing.T) {
	root := t.TempDir()
	genTree(t, root, 32, 600, 1200)
	time.Sleep(50 * time.Millisecond)
	base := runtime.NumGoroutine()

	s, err := New(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	s.Pause() // paused before Run: the very first job blocks at the gate
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := s.Run(ctx, func(proto.Record) {})
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("Run returned while paused: %v", err)
	default:
	}
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("err=%v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after cancel while paused")
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("cancel took %v", el)
	}
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > base && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if g := runtime.NumGoroutine(); g > base {
		t.Fatalf("goroutines %d > baseline %d", g, base)
	}
}
