// SPDX-License-Identifier: MIT

//go:build linux

package scan

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// isMountRoot reports whether name in dfd is the root of a mount (including a
// bind mount on the same device), when the kernel can tell (Linux 5.8+).
func isMountRoot(dfd int, name string) bool {
	var stx unix.Statx_t
	if unix.Statx(dfd, name, unix.AT_SYMLINK_NOFOLLOW|unix.AT_STATX_DONT_SYNC, unix.STATX_TYPE, &stx) != nil {
		return false
	}
	return stx.Attributes_mask&unix.STATX_ATTR_MOUNT_ROOT != 0 && stx.Attributes&unix.STATX_ATTR_MOUNT_ROOT != 0
}

const (
	maxTrashTries = 1000
	infoSuffix    = ".trashinfo"
)

// trash moves the pinned target into a freedesktop.org Trash on the target's
// filesystem: $XDG_DATA_HOME/Trash (default <home>/.local/share/Trash) when
// that is on the same device, else $topdir/.Trash/$uid (.Trash sticky, not a
// symlink) or $topdir/.Trash-$uid. info/<n>.trashinfo is created with O_EXCL
// first, then the target is renamed with RENAME_NOREPLACE into files/<n>, both
// relative to no-follow directory fds; the info file is removed if the move
// fails. Nothing crosses devices: with no usable Trash the result is
// trash_unavailable.
func (r *remover) trash(p *pinned, t *Target) (string, error) {
	home, hst, err := homeStat(r.opts.Home)
	if err != nil {
		return "", rmErr(CodeTrashUnavailable, t.Path, err)
	}
	uid, gid := int(hst.Uid), int(hst.Gid)
	dataHome := os.Getenv("XDG_DATA_HOME")
	if !filepath.IsAbs(dataHome) {
		dataHome = filepath.Join(home, ".local", "share")
	}
	var dir, topdir string
	if dev, ok := nearestDev(dataHome); ok && dev == t.ID.Dev {
		if err := mkdirChain(dataHome, uid, gid); err != nil {
			return "", rmErr(CodeTrashUnavailable, t.Path, err)
		}
		dir = filepath.Join(dataHome, "Trash")
		if err := ensureDir(dir, 0o700, uid, gid); err != nil {
			return "", rmErr(CodeTrashUnavailable, t.Path, err)
		}
	} else {
		topdir = mountTop(p.canon, t.ID.Dev)
		if dir = topTrash(topdir, uid, gid); dir == "" {
			return "", rmErr(CodeTrashUnavailable, t.Path, errors.New("no usable Trash on this filesystem"))
		}
	}
	tfd, err := openDir(dir)
	if err != nil {
		return "", rmErr(CodeTrashUnavailable, t.Path, err)
	}
	defer unix.Close(tfd)
	filesFd, err := trashSubdir(tfd, "files", uid, gid, t.ID.Dev)
	if err != nil {
		return "", rmErr(CodeTrashUnavailable, t.Path, err)
	}
	defer unix.Close(filesFd)
	infoFd, err := trashSubdir(tfd, "info", uid, gid, t.ID.Dev)
	if err != nil {
		return "", rmErr(CodeTrashUnavailable, t.Path, err)
	}
	defer unix.Close(infoFd)

	orig := p.canon
	if topdir != "" {
		orig = strings.TrimPrefix(strings.TrimPrefix(orig, topdir), "/")
	}
	info := "[Trash Info]\nPath=" + trashInfoPath(orig) + "\nDeletionDate=" + time.Now().Format("2006-01-02T15:04:05") + "\n"
	for n := 1; n <= maxTrashTries; n++ {
		if r.canceled() {
			return "", rmErr(CodeCanceled, t.Path, r.ctx.Err())
		}
		nm := trashName(t.Name, n, t.Dir, 255-len(infoSuffix))
		ifd, err := unix.Openat(infoFd, nm+infoSuffix, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return "", rmErr(CodeTrashUnavailable, t.Path, err)
		}
		if unix.Geteuid() == 0 && uid != 0 {
			unix.Fchown(ifd, uid, gid)
		}
		_, werr := unix.Write(ifd, []byte(info))
		if cerr := unix.Close(ifd); werr == nil {
			werr = cerr
		}
		if werr != nil {
			unlinkat(infoFd, nm+infoSuffix, 0)
			return "", rmErr(CodeTrashUnavailable, t.Path, werr)
		}
		err = unix.Renameat2(p.pfd, t.Name, filesFd, nm, unix.RENAME_NOREPLACE)
		if err != nil {
			unlinkat(infoFd, nm+infoSuffix, 0)
			switch {
			case errors.Is(err, unix.EEXIST):
				continue
			case errors.Is(err, unix.EXDEV), errors.Is(err, unix.EINVAL), errors.Is(err, unix.ENOSYS):
				// Another device, or no RENAME_NOREPLACE on this filesystem.
				return "", rmErr(CodeTrashUnavailable, t.Path, err)
			}
			return "", rmErr(errCode(err), t.Path, err)
		}
		where := filepath.Join(dir, "files", nm)
		var st unix.Stat_t
		if err := fstatat(filesFd, nm, &st); err != nil || statIdentity(&st) != t.ID {
			return where, rmErr(CodeChanged, t.Path, errors.New("the entry moved to the Trash is not the target"))
		}
		return where, nil
	}
	return "", rmErr(CodeTrashUnavailable, t.Path, errors.New("no free name in the Trash"))
}

// trashSubdir opens (creating if needed) files/ or info/ in the Trash fd,
// without following a symlink, and checks owner and device.
func trashSubdir(tfd int, name string, uid, gid int, dev uint64) (int, error) {
	err := unix.Mkdirat(tfd, name, 0o700)
	created := err == nil
	if err != nil && !errors.Is(err, unix.EEXIST) {
		return -1, err
	}
	fd, err := openatDir(tfd, name)
	if err != nil {
		return -1, err
	}
	if created && unix.Geteuid() == 0 && uid != 0 {
		unix.Fchown(fd, uid, gid)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return -1, err
	}
	if int(st.Uid) != uid || devOf(&st) != dev {
		unix.Close(fd)
		return -1, errors.New("Trash " + name + " is not usable")
	}
	return fd, nil
}

// nearestDev returns the device of path or of its nearest existing ancestor.
func nearestDev(path string) (uint64, bool) {
	for {
		var st unix.Stat_t
		err := unix.Stat(path, &st)
		if err == nil {
			return devOf(&st), true
		}
		up := filepath.Dir(path)
		if !errors.Is(err, unix.ENOENT) || up == path {
			return 0, false
		}
		path = up
	}
}

// mkdirChain creates the missing directories of path (0700, owned by uid).
func mkdirChain(path string, uid, gid int) error {
	var missing []string
	for q := path; ; {
		var st unix.Stat_t
		if unix.Lstat(q, &st) == nil {
			break
		}
		missing = append(missing, q)
		up := filepath.Dir(q)
		if up == q {
			break
		}
		q = up
	}
	for i := len(missing) - 1; i >= 0; i-- {
		if err := ensureDir(missing[i], 0o700, uid, gid); err != nil {
			return err
		}
	}
	return nil
}

// mountTop returns the topmost ancestor of the entry at canon that is still on
// device dev: the mount's top directory.
func mountTop(canon string, dev uint64) string {
	q := filepath.Dir(canon)
	for {
		up := filepath.Dir(q)
		if up == q {
			return q
		}
		var st unix.Stat_t
		if unix.Stat(up, &st) != nil || devOf(&st) != dev {
			return q
		}
		q = up
	}
}

// topTrash returns the usable per-user Trash in topdir, creating it if
// needed, or "".
func topTrash(topdir string, uid, gid int) string {
	shared := filepath.Join(topdir, ".Trash")
	var st unix.Stat_t
	if unix.Lstat(shared, &st) == nil && st.Mode&unix.S_IFMT == unix.S_IFDIR && st.Mode&unix.S_ISVTX != 0 {
		d := filepath.Join(shared, strconv.Itoa(uid))
		if ensureDir(d, 0o700, uid, gid) == nil {
			return d
		}
	}
	d := filepath.Join(topdir, ".Trash-"+strconv.Itoa(uid))
	if ensureDir(d, 0o700, uid, gid) == nil {
		return d
	}
	return ""
}

// trashInfoPath percent-encodes a path for the Path= key: every byte except
// unreserved characters and "/" (RFC 2396 escaping as the spec asks).
func trashInfoPath(p string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("-._~/", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&15])
	}
	return b.String()
}
