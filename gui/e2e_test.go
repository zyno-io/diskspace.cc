// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestE2ERealAgent drives the real `Diskspace agent` child against a local
// relay. Opt-in: DISKSPACE_GUI_E2E_BIN=<built gui binary>
// DISKSPACE_GUI_E2E_RELAY=http://127.0.0.1:<port>. It never scans: no viewer
// connects, and the offered home is an empty temp dir.
func TestE2ERealAgent(t *testing.T) {
	bin, relay := os.Getenv("DISKSPACE_GUI_E2E_BIN"), os.Getenv("DISKSPACE_GUI_E2E_RELAY")
	if bin == "" || relay == "" {
		t.Skip("set DISKSPACE_GUI_E2E_BIN and DISKSPACE_GUI_E2E_RELAY")
	}
	ch := make(chan Snapshot, 64)
	a := &App{version: "e2e", relay: relay, self: bin, home: t.TempDir(), spawn: defaultLaunch, snap: Snapshot{Phase: phaseIdle}}
	a.emitFn = func(s Snapshot) { ch <- s }
	if err := a.Start(false, false); err != nil {
		t.Fatal(err)
	}
	wait := func(ok func(Snapshot) bool) Snapshot {
		t.Helper()
		deadline := time.After(20 * time.Second)
		for {
			select {
			case s := <-ch:
				if s.Phase == phaseError {
					t.Fatalf("agent error: %s", s.Message)
				}
				if ok(s) {
					return s
				}
			case <-deadline:
				t.Fatal("timeout")
			}
		}
	}
	s := wait(func(s Snapshot) bool { return s.URL != "" })
	t.Logf("ready: url=%s… local=%s… elevated=%v fda=%q", s.URL[:len(relay)+8], s.LocalURL[:30], s.Elevated, s.FDA)
	if _, ok := embeddedURL(s.LocalURL); !ok {
		t.Errorf("local URL not embeddable: %q", s.LocalURL)
	}
	first := s.URL
	if err := a.Rotate(); err != nil {
		t.Fatal(err)
	}
	s = wait(func(s Snapshot) bool { return s.URL != "" && s.URL != first })
	t.Logf("rotated: %s…", s.URL[:len(relay)+8])

	a.mu.Lock()
	exited := a.cur.h.exited
	a.mu.Unlock()
	start := time.Now()
	a.shutdown(context.Background())
	select {
	case <-exited:
		t.Logf("child exited %v after shutdown", time.Since(start).Round(time.Millisecond))
	case <-time.After(3 * time.Second):
		t.Fatal("child still running after shutdown")
	}
}
