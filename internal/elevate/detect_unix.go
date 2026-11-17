// SPDX-License-Identifier: MIT

//go:build !windows

package elevate

import (
	"os"
	"syscall"
)

// IsElevated reports whether the process runs as root.
func IsElevated() bool { return os.Geteuid() == 0 }

// ParentAlive reports whether pid exists.
func ParentAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
