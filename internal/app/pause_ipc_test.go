// SPDX-License-Identifier: MIT

package app

import (
	"testing"

	"github.com/zyno-io/diskspace.cc/internal/session"
)

// Pause and resume events survive the elevated-child IPC round trip.
func TestIPCPauseEvents(t *testing.T) {
	for _, k := range []session.EventKind{session.EvScanPaused, session.EvScanResumed} {
		m, ok := eventIPC(session.Event{Kind: k, Path: "/x"}, false, "")
		if !ok || m.T != "scan" {
			t.Fatalf("kind %d: eventIPC = %+v, %v", k, m, ok)
		}
		ev, ok := ipcEvent(m)
		if !ok || ev.Kind != k || ev.Path != "/x" {
			t.Fatalf("kind %d: ipcEvent = %+v, %v", k, ev, ok)
		}
	}
}
