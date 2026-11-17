// SPDX-License-Identifier: MIT

// Package prio lowers the CPU and I/O priority of the scanner so a large scan
// does not starve the desktop (window server, browser, input). Everything is
// best effort: failures are returned but never fatal. No cgo.
package prio

import "strings"

// Level is a scheduling priority preset.
type Level int

const (
	// Normal leaves priorities untouched (the zero value, so library users
	// that never set a level are unaffected).
	Normal Level = iota
	// Low is below-normal CPU and low-tier disk I/O.
	Low
)

// Parse converts a flag value ("low" or "normal") to a Level.
func Parse(s string) (Level, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "low":
		return Low, true
	case "normal":
		return Normal, true
	}
	return Normal, false
}

func (l Level) String() string {
	if l == Low {
		return "low"
	}
	return "normal"
}

// ApplyProcess applies the level to the whole process (macOS, Windows). On
// Linux priorities are per thread; see LockThreadLow.
func ApplyProcess(level Level) error {
	if level != Low {
		return nil
	}
	return applyProcessLow()
}

// LockThreadLow pins the calling goroutine to its OS thread and lowers that
// thread's priority (Linux only; no-op elsewhere). The thread is never
// unlocked, so it is destroyed when the goroutine exits and the priority
// cannot leak to unrelated goroutines.
func LockThreadLow() error {
	return lockThreadLow()
}
