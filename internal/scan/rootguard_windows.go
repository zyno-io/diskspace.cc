// SPDX-License-Identifier: MIT

//go:build windows

package scan

import (
	"errors"
	"syscall"

	"golang.org/x/sys/windows"
)

// openFollow opens a directory handle for attribute queries, following
// reparse points in every component (the handle names the real target).
func openFollow(path string) (windows.Handle, error) {
	p := wtf8ToUTF16(toLongPath(path))
	return windows.CreateFile(&p[0], windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
}

// finalPath returns the canonical DOS path of an open handle.
func finalPath(h windows.Handle) (string, error) {
	buf := make([]uint16, 512)
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0) // VOLUME_NAME_DOS
		if err != nil {
			return "", err
		}
		if int(n) <= len(buf) {
			return syscall.UTF16ToString(buf[:n]), nil
		}
		buf = make([]uint16, n+1)
	}
}

func (g *RootGuard) initRoot(real string) error {
	h, err := openFollow(real)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	id, ok := handleIdentity(h)
	if !ok {
		return errors.New("scan: cannot identify root")
	}
	if g.canon, err = finalPath(h); err != nil {
		return err
	}
	g.id = id
	return nil
}

// contained opens resolved, derives both the identity and the canonical final
// path from that one handle, and checks the path against the root's.
func (g *RootGuard) contained(resolved string) (Identity, bool) {
	h, err := openFollow(resolved)
	if err != nil {
		return Identity{}, false
	}
	defer windows.CloseHandle(h)
	id, ok := handleIdentity(h)
	if !ok {
		return Identity{}, false
	}
	fp, err := finalPath(h)
	if err != nil || !pathWithin(g.canon, fp) {
		return Identity{}, false
	}
	return id, true
}
