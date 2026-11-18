// SPDX-License-Identifier: MIT

//go:build darwin

package scan

import (
	"errors"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"
)

func isMountRoot(dfd int, name string) bool { return false }

// maxTrashTries bounds the " 2", " 3"... collision suffixes.
const maxTrashTries = 1000

// trash moves the pinned target into the user's Trash: <home>/.Trash, or
// <volume>/.Trashes/<uid> for a target on another volume.
//
// The Trash is never opened: without Full Disk Access, open(~/.Trash) fails
// with EPERM (TCC) while a rename into it by path still works. So the move is
// renameatx_np(parent fd, name, AT_FDCWD, "<trash>/<name>", RENAME_EXCL) and
// the result is verified afterwards: the destination must have the identity
// of the target. A rename never crosses volumes and never replaces anything.
// No Finder "Put Back" metadata is written.
func (r *remover) trash(p *pinned, t *Target) (string, error) {
	home, hst, err := homeStat(r.opts.Home)
	if err != nil {
		return "", rmErr(CodeTrashUnavailable, t.Path, err)
	}
	uid, gid := int(hst.Uid), int(hst.Gid)
	var dir string
	if devOf(hst) == t.ID.Dev {
		dir = filepath.Join(home, ".Trash")
		if err := ensureDir(dir, 0o700, uid, gid); err != nil {
			return "", rmErr(CodeTrashUnavailable, t.Path, err)
		}
	} else {
		var fs unix.Statfs_t
		if err := unix.Fstatfs(p.pfd, &fs); err != nil {
			return "", rmErr(CodeTrashUnavailable, t.Path, err)
		}
		mnt := cstr8(fs.Mntonname[:])
		if mnt == "" || mnt == "/" {
			return "", rmErr(CodeTrashUnavailable, t.Path, errors.New("no Trash on the system volume"))
		}
		top := filepath.Join(mnt, ".Trashes")
		if err := ensureDir(top, 0o1333, -1, -1); err != nil {
			return "", rmErr(CodeTrashUnavailable, t.Path, err)
		}
		dir = filepath.Join(top, strconv.Itoa(uid))
		if err := ensureDir(dir, 0o700, uid, gid); err != nil {
			return "", rmErr(CodeTrashUnavailable, t.Path, err)
		}
		var st unix.Stat_t
		if err := unix.Lstat(dir, &st); err != nil || devOf(&st) != t.ID.Dev {
			return "", rmErr(CodeTrashUnavailable, t.Path, errors.New("Trash is on another volume"))
		}
	}
	for n := 1; n <= maxTrashTries; n++ {
		if r.canceled() {
			return "", rmErr(CodeCanceled, t.Path, r.ctx.Err())
		}
		dest := filepath.Join(dir, trashName(t.Name, n, t.Dir, 255))
		err := unix.RenameatxNp(p.pfd, t.Name, unix.AT_FDCWD, dest, unix.RENAME_EXCL)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if errors.Is(err, unix.EXDEV) {
			return "", rmErr(CodeTrashUnavailable, t.Path, err)
		}
		if err != nil {
			return "", rmErr(errCode(err), t.Path, err)
		}
		var st unix.Stat_t
		if err := unix.Lstat(dest, &st); err != nil || statIdentity(&st) != t.ID {
			return dest, rmErr(CodeChanged, t.Path, errors.New("the entry moved to the Trash is not the target"))
		}
		return dest, nil
	}
	return "", rmErr(CodeTrashUnavailable, t.Path, errors.New("no free name in the Trash"))
}
