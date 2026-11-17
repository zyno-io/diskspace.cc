// SPDX-License-Identifier: MIT

//go:build linux

package scan

import "golang.org/x/sys/unix"

// swapPaths atomically exchanges two directory entries.
func swapPaths(a, b string) error {
	return unix.Renameat2(unix.AT_FDCWD, a, unix.AT_FDCWD, b, unix.RENAME_EXCHANGE)
}
