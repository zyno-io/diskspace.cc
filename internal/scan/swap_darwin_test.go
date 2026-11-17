// SPDX-License-Identifier: MIT

//go:build darwin

package scan

import "golang.org/x/sys/unix"

// swapPaths atomically exchanges two directory entries.
func swapPaths(a, b string) error { return unix.RenamexNp(a, b, unix.RENAME_SWAP) }
