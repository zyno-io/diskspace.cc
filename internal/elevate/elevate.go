// SPDX-License-Identifier: MIT

// Package elevate detects, requests and supervises administrator privileges.
package elevate

import (
	"context"
	"errors"
	"os"
	"os/user"
	"time"
)

var (
	// ErrNoTTY is returned when no controlling terminal is available.
	ErrNoTTY = errors.New("no terminal available")
	// ErrCancelled is returned when the user declined the OS elevation dialog.
	ErrCancelled = errors.New("elevation cancelled by user")
	// ErrUnsupported is returned on platforms where an operation does not apply.
	ErrUnsupported = errors.ErrUnsupported
)

// FDAState is the macOS Full Disk Access state of the current process.
type FDAState string

const (
	FDAGranted FDAState = "granted"
	FDADenied  FDAState = "denied"
	FDAUnknown FDAState = ""
)

// Child is a handle to an elevated child process (Windows).
type Child interface {
	Wait() (exitCode int, err error)
	Pid() int
	Kill() error
}

// CurrentUser returns the username of the current process owner.
func CurrentUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	for _, k := range []string{"USER", "USERNAME", "LOGNAME"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return "unknown"
}

// InvokingUser returns SUDO_USER when running under sudo, else CurrentUser.
func InvokingUser() string {
	if v := os.Getenv("SUDO_USER"); v != "" && IsElevated() {
		return v
	}
	return CurrentUser()
}

// WatchParent polls ParentAlive every 2s and calls onGone once when the
// parent is gone. It returns when ctx is done or the parent disappears.
func WatchParent(ctx context.Context, pid int, onGone func()) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !ParentAlive(pid) {
				onGone()
				return
			}
		}
	}
}
