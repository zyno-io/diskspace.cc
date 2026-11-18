// SPDX-License-Identifier: MIT

//go:build darwin || linux

package scan

import (
	"errors"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const badNameChars = "/\x00"

// pinned is a target resolved for one operation: the verified parent, and for
// a directory the verified directory itself.
type pinned struct {
	pfd   int
	fd    int // directory targets; -1 otherwise
	pdev  uint64
	st    unix.Stat_t // the target (lstat for files, fstat for directories)
	canon string      // canonical path of the target
}

func (p *pinned) close() {
	if p.fd >= 0 {
		unix.Close(p.fd)
	}
	unix.Close(p.pfd)
}

func statIdentity(st *unix.Stat_t) Identity {
	return Identity{Dev: devOf(st), Ino: uint64(st.Ino)}
}

func pathIdentity(path string) (Identity, bool) {
	id, err := rootID(path)
	return id, err == nil
}

// errCode maps an errno to a removal code.
func errCode(err error) string {
	switch {
	case errors.Is(err, unix.ENOENT):
		return CodeNotFound
	case errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENOTDIR):
		// O_NOFOLLOW met a symlink, or a directory was replaced by a file.
		return CodeChanged
	case isDenied(err), errors.Is(err, unix.EROFS):
		return CodePermission
	case errors.Is(err, unix.EXDEV), errors.Is(err, unix.EBUSY):
		return CodeMountPoint
	case errors.Is(err, unix.ENAMETOOLONG):
		return CodeInvalidName
	}
	return CodeFailed
}

func fstatat(dfd int, name string, st *unix.Stat_t) error {
	for {
		err := unix.Fstatat(dfd, name, st, unix.AT_SYMLINK_NOFOLLOW)
		if err != unix.EINTR {
			return err
		}
	}
}

func unlinkat(dfd int, name string, flags int) error {
	for {
		err := unix.Unlinkat(dfd, name, flags)
		if err != unix.EINTR {
			return err
		}
	}
}

// pin opens t's parent by path, verifies it against the recorded identity,
// proves containment by walking ".." through the recorded ancestors to the
// scan root (and the guard root), then examines the target relative to the
// parent fd without following a symlink.
func pin(t *Target) (*pinned, error) {
	if err := preflightLoc(t); err != nil {
		return nil, err
	}
	b := &t.bounds
	pfd, err := openDir(t.Parent)
	if err != nil {
		return nil, rmErr(errCode(err), t.Parent, err)
	}
	p := &pinned{pfd: pfd, fd: -1}
	ok := false
	defer func() {
		if !ok {
			p.close()
		}
	}()
	var pst unix.Stat_t
	if err := unix.Fstat(pfd, &pst); err != nil {
		return nil, rmErr(CodeFailed, t.Parent, err)
	}
	p.pdev = devOf(&pst)
	if statIdentity(&pst) != t.Chain[0] {
		return nil, rmErr(CodeChanged, t.Parent, nil)
	}
	stops := append(append([]Identity(nil), t.Chain[1:]...), b.RootID)
	if b.Guard != nil {
		stops = append(stops, b.Guard.id)
	}
	if !walkUp(pfd, stops...) {
		return nil, rmErr(CodeOutsideRoot, t.Parent, nil)
	}
	if t.Dir {
		fd, err := openatDir(pfd, t.Name)
		if err != nil {
			return nil, rmErr(errCode(err), t.Path, err)
		}
		p.fd = fd
		if err := unix.Fstat(fd, &p.st); err != nil {
			return nil, rmErr(CodeFailed, t.Path, err)
		}
	} else {
		if err := fstatat(pfd, t.Name, &p.st); err != nil {
			return nil, rmErr(errCode(err), t.Path, err)
		}
		if p.st.Mode&unix.S_IFMT == unix.S_IFDIR {
			return nil, rmErr(CodeChanged, t.Path, errors.New("is a directory"))
		}
	}
	id := statIdentity(&p.st)
	if !t.ID.IsZero() && id != t.ID {
		return nil, rmErr(CodeChanged, t.Path, nil)
	}
	if id.Dev != p.pdev || isMountRoot(pfd, t.Name) {
		return nil, rmErr(CodeMountPoint, t.Path, nil)
	}
	p.canon = t.Path
	if real, err := filepath.EvalSymlinks(t.Parent); err == nil {
		p.canon = joinPath(real, t.Name)
	}
	if t.Dir {
		if id == b.RootID {
			return nil, rmErr(CodeProtected, t.Path, errors.New("scan root"))
		}
		if b.protection().covers(p.canon, id) {
			return nil, rmErr(CodeProtected, t.Path, nil)
		}
	} else {
		t.ID = id
		t.Alloc, t.Apparent, t.Links = uint64(p.st.Blocks)*512, uint64(p.st.Size), uint64(p.st.Nlink)
	}
	ok = true
	return p, nil
}

func (r *remover) removeFile(p *pinned, t *Target) bool {
	if err := unlinkat(p.pfd, t.Name, 0); err != nil {
		r.fail(t.Path, errCode(err), err)
		return false
	}
	r.removed(false, t.Alloc, t.Apparent, t.Links <= 1)
	return true
}

func (r *remover) removeTree(p *pinned, t *Target) bool {
	return r.removeDirAt(p.pfd, t.Name, t.Path, t.ID, 0)
}

// removeChunk is how many names are read before removing them; after any
// removal the directory is re-opened, since its read offset is no longer
// reliable (macOS emulates getdirentries by counting entries).
const removeChunk = 1024

// removeDirAt removes directory name in pfd, which must have identity want,
// and everything in it on the same filesystem. Like os.RemoveAll (removeall_at)
// but every open is O_NOFOLLOW and re-verified, other filesystems and mount
// roots are skipped, and failures are collected per entry. It reports whether
// the directory itself was removed.
func (r *remover) removeDirAt(pfd int, name, path string, want Identity, depth int) bool {
	if depth > maxRemoveDepth {
		r.fail(path, CodeFailed, errTooDeep)
		return false
	}
	r.beforeOpen(path)
	var (
		failed map[string]struct{} // left in place; never retried
		self   unix.Stat_t
		buf    = make([]byte, 8<<10)
		names  []string
	)
	for {
		if r.canceled() {
			return false
		}
		fd, err := openatDir(pfd, name)
		if err != nil {
			r.fail(path, errCode(err), err)
			return false
		}
		if err := unix.Fstat(fd, &self); err != nil || statIdentity(&self) != want {
			unix.Close(fd)
			r.fail(path, CodeChanged, err)
			return false
		}
		dev := devOf(&self)
		progress, eof := false, false
		for !progress && !eof {
			names, eof, err = readNames(fd, buf, names[:0])
			if err != nil {
				unix.Close(fd)
				r.fail(path, errCode(err), err)
				return false
			}
			for _, n := range names {
				if r.canceled() {
					unix.Close(fd)
					return false
				}
				if _, bad := failed[n]; bad {
					continue
				}
				if r.removeEntry(fd, n, joinPath(path, n), dev, depth) {
					progress = true
					continue
				}
				if failed == nil {
					failed = map[string]struct{}{}
				}
				failed[n] = struct{}{}
			}
		}
		unix.Close(fd)
		if !progress {
			break
		}
	}
	if len(failed) > 0 {
		return false // the reasons were recorded for the entries left behind
	}
	if err := unlinkat(pfd, name, unix.AT_REMOVEDIR); err != nil {
		r.fail(path, errCode(err), err)
		return false
	}
	r.removed(true, uint64(self.Blocks)*512, uint64(self.Size), true)
	return true
}

// removeEntry removes one entry of directory dfd (on device dev). It reports
// whether the entry is gone.
func (r *remover) removeEntry(dfd int, name, path string, dev uint64, depth int) bool {
	var st unix.Stat_t
	if err := fstatat(dfd, name, &st); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return true
		}
		r.fail(path, errCode(err), err)
		return false
	}
	if st.Mode&unix.S_IFMT == unix.S_IFDIR {
		if r.devOf(path, devOf(&st)) != dev || isMountRoot(dfd, name) {
			r.skip(path)
			return false
		}
		return r.removeDirAt(dfd, name, path, statIdentity(&st), depth+1)
	}
	// A symlink is removed as the link itself.
	if err := unlinkat(dfd, name, 0); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return true
		}
		r.fail(path, errCode(err), err)
		return false
	}
	r.removed(false, uint64(st.Blocks)*512, uint64(st.Size), st.Nlink <= 1)
	return true
}

// readNames reads at least removeChunk names (or to the end) from fd.
func readNames(fd int, buf []byte, names []string) ([]string, bool, error) {
	for len(names) < removeChunk {
		var n int
		var err error
		for {
			n, err = unix.ReadDirent(fd, buf)
			if err != unix.EINTR {
				break
			}
		}
		if err != nil {
			return names, false, err
		}
		if n <= 0 {
			return names, true, nil
		}
		_, _, names = unix.ParseDirent(buf[:n], -1, names)
	}
	return names, false, nil
}

// ensureDir makes sure path is a real directory (not a symlink), creating it
// with mode if missing, owned by uid (when uid >= 0; created directories are
// handed to uid/gid when running as root).
func ensureDir(path string, mode uint32, uid, gid int) error {
	var st unix.Stat_t
	err := unix.Lstat(path, &st)
	if errors.Is(err, unix.ENOENT) {
		if err := unix.Mkdir(path, mode); err != nil && !errors.Is(err, unix.EEXIST) {
			return err
		}
		fixMode(path, mode)
		if uid >= 0 && unix.Geteuid() == 0 && uid != 0 {
			unix.Lchown(path, uid, gid)
		}
		err = unix.Lstat(path, &st)
	}
	if err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errors.New(path + " is not a directory")
	}
	if uid >= 0 && int(st.Uid) != uid {
		return errors.New(path + " is owned by another user")
	}
	return nil
}

// fixMode applies mode bits the umask removed (e.g. sticky, world-writable)
// through a no-follow fd. Best effort.
func fixMode(path string, mode uint32) {
	fd, err := openDir(path)
	if err != nil {
		return
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if unix.Fstat(fd, &st) == nil && uint32(st.Mode)&07777 != mode {
		unix.Fchmod(fd, mode)
	}
}

// homeStat returns the home directory and its stat (owner, device).
func homeStat(h string) (string, *unix.Stat_t, error) {
	home, err := homeDir(h)
	if err != nil {
		return "", nil, err
	}
	var st unix.Stat_t
	if err := unix.Stat(home, &st); err != nil {
		return "", nil, err
	}
	return home, &st, nil
}
