// SPDX-License-Identifier: MIT

//go:build windows

package elevate

import "golang.org/x/sys/windows"

// IsElevated reports whether the process token is elevated.
func IsElevated() bool { return windows.GetCurrentProcessToken().IsElevated() }

// ParentAlive reports whether pid is still running.
func ParentAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		// Access denied means the process exists but is protected.
		return err == windows.ERROR_ACCESS_DENIED
	}
	defer windows.CloseHandle(h)
	ev, err := windows.WaitForSingleObject(h, 0)
	return err == nil && ev == uint32(windows.WAIT_TIMEOUT)
}
