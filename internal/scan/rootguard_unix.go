// SPDX-License-Identifier: MIT

//go:build darwin || linux

package scan

import "golang.org/x/sys/unix"

// maxGuardWalk bounds the upward ".." walk.
const maxGuardWalk = 4096

func fdIdentity(fd int) (Identity, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return Identity{}, err
	}
	return Identity{Dev: devOf(&st), Ino: uint64(st.Ino)}, nil
}

func openatDir(dirfd int, name string) (int, error) {
	for {
		fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err == unix.EINTR {
			continue
		}
		return fd, err
	}
}

func (g *RootGuard) initRoot(real string) error {
	fd, err := openDir(real)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	g.id, err = fdIdentity(fd)
	return err
}

// contained opens resolved, takes its identity from the handle, and walks
// upward through ".." from that same handle until it meets the guard root
// (contained) or the filesystem root (not contained).
func (g *RootGuard) contained(resolved string) (Identity, bool) {
	fd, err := openDir(resolved)
	if err != nil {
		return Identity{}, false
	}
	id, err := fdIdentity(fd)
	if err != nil {
		unix.Close(fd)
		return Identity{}, false
	}
	cur := id
	for i := 0; i < maxGuardWalk; i++ {
		if cur == g.id {
			unix.Close(fd)
			return id, true
		}
		pfd, err := openatDir(fd, "..")
		unix.Close(fd)
		if err != nil {
			return Identity{}, false
		}
		fd = pfd
		pid, err := fdIdentity(fd)
		if err != nil || pid == cur {
			// error, or ".." of the filesystem root is itself: not contained.
			unix.Close(fd)
			return Identity{}, false
		}
		cur = pid
	}
	unix.Close(fd)
	return Identity{}, false
}
