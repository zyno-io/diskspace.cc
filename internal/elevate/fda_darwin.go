// SPDX-License-Identifier: MIT

//go:build darwin

package elevate

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// ProbeFDA checks whether this process has Full Disk Access.
func ProbeFDA() FDAState {
	_, err := os.ReadDir("/Library/Application Support/com.apple.TCC")
	switch {
	case err == nil:
		return FDAGranted
	case errors.Is(err, syscall.EPERM), errors.Is(err, syscall.EACCES), errors.Is(err, os.ErrPermission):
		return FDADenied
	}
	return FDAUnknown
}

// OpenFDASettings opens the Full Disk Access pane in System Settings.
func OpenFDASettings() error {
	return exec.Command("open", "x-apple.systempreferences:com.apple.preference.security?Privacy_AllFiles").Run()
}
