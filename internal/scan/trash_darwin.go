// SPDX-License-Identifier: MIT

//go:build darwin

package scan

import (
	"errors"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"
)

// isMountRoot: on macOS every mount has its own device, so the device
// comparison already finds mount points.
func isMountRoot(dfd int, name, canon string, mounts func() map[string]struct{}) bool { return false }

func freshMounts() func() map[string]struct{} { return nil }

// maxTrashTries bounds the " 2", " 3"... collision suffixes.
const maxTrashTries = 1000

// trash moves the pinned target into the user's Trash: <home>/.Trash, or
// <volume>/.Trashes/<uid> for a target on another volume.
//
// ~/.Trash is never opened: without Full Disk Access, open(~/.Trash) fails
// with EPERM (TCC) while a rename into it by path still works. So that move
// is renameatx_np(parent fd, name, AT_FDCWD, "~/.Trash/<name>", RENAME_EXCL)
// and the result is verified afterwards: the destination must have the
// identity of the target. On other volumes .Trashes (root- or user-owned,
// sticky) and <uid> (the user's, 0700) are opened without following links and
// the rename is relative to the <uid> fd. A rename never crosses volumes and
// never replaces anything. No Finder "Put Back" metadata is written.
func (r *remover) trash(p *pinned, t *Target) (string, error) {
	home, hst, err := homeStat(r.opts.Home)
	if err != nil {
		return "", rmErr(CodeTrashUnavailable, t.Path, err)
	}
	uid, gid := int(hst.Uid), int(hst.Gid)
	var dir string
	dirFd := unix.AT_FDCWD
	if devOf(hst) == t.ID.Dev {
		dir = filepath.Join(home, ".Trash")
		if err := ensureDir(dir, 0o700, uid, gid); err != nil {
			return "", rmErr(CodeTrashUnavailable, t.Path, err)
		}
	} else {
		fd, d, err := volumeTrash(p.pfd, uid, gid, t.ID.Dev)
		if err != nil {
			return "", rmErr(CodeTrashUnavailable, t.Path, err)
		}
		defer unix.Close(fd)
		dirFd, dir = fd, d
	}
	for n := 1; n <= maxTrashTries; n++ {
		if r.canceled() {
			return "", rmErr(CodeCanceled, t.Path, r.ctx.Err())
		}
		name := trashName(t.Name, n, t.Dir, 255)
		dest := filepath.Join(dir, name)
		to := dest
		if dirFd != unix.AT_FDCWD {
			to = name
		}
		err := unix.RenameatxNp(p.pfd, t.Name, dirFd, to, unix.RENAME_EXCL)
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
		if err := fstatat(dirFd, to, &st); err != nil || statIdentity(&st) != t.ID {
			return dest, rmErr(CodeChanged, t.Path, errors.New("the entry moved to the Trash is not the target"))
		}
		return dest, nil
	}
	return "", rmErr(CodeTrashUnavailable, t.Path, errors.New("no free name in the Trash"))
}

// volumeTrash opens <volume>/.Trashes/<uid> for the volume holding pfd,
// creating it per Apple's conventions (.Trashes 01333, <uid> 0700) if needed.
func volumeTrash(pfd, uid, gid int, dev uint64) (int, string, error) {
	var fs unix.Statfs_t
	if err := unix.Fstatfs(pfd, &fs); err != nil {
		return -1, "", err
	}
	mnt := cstr8(fs.Mntonname[:])
	if mnt == "" || mnt == "/" {
		return -1, "", errors.New("no Trash on the system volume")
	}
	top := filepath.Join(mnt, ".Trashes")
	if err := ensureDir(top, 0o1333, -1, -1); err != nil {
		return -1, "", err
	}
	tfd, err := openDir(top)
	if err != nil {
		return -1, "", err
	}
	defer unix.Close(tfd)
	var st unix.Stat_t
	if err := unix.Fstat(tfd, &st); err != nil {
		return -1, "", err
	}
	if st.Uid != 0 && int(st.Uid) != uid || st.Mode&unix.S_ISVTX == 0 || devOf(&st) != dev {
		return -1, "", errors.New(top + " is not usable")
	}
	fd, err := ensureSubdir(tfd, strconv.Itoa(uid), uid, gid, dev)
	if err != nil {
		return -1, "", err
	}
	return fd, filepath.Join(top, strconv.Itoa(uid)), nil
}
