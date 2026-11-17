// SPDX-License-Identifier: MIT

//go:build !darwin

package elevate

// SpawnAdminDarwin is macOS-only.
func SpawnAdminDarwin(self string, args []string) error { return ErrUnsupported }
