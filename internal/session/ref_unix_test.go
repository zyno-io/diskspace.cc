// SPDX-License-Identifier: MIT

//go:build !windows

package session

import (
	"io/fs"
	"path/filepath"
	"syscall"
	"testing"
)

// refAlloc is an independent allocated-bytes total (files and dirs), valid for
// hardlink-free trees.
func refAlloc(t *testing.T, root string) int64 {
	t.Helper()
	var total int64
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		total += fi.Sys().(*syscall.Stat_t).Blocks * 512
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return total
}
